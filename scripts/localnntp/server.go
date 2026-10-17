// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/test/mocknntp"
)

// ServerConfig configures the localnntp disk-backed NNTP server.
type ServerConfig struct {
	// Releases are the releases served together. Each keeps its own articles.pack and files/
	// directory; Message-IDs must be unique across all of them. At least one is required.
	Releases []ReleaseSource
	// ListenAddr is the TCP address to bind, defaults to "127.0.0.1:0" if empty.
	ListenAddr string
	// Faults is the fault injection engine; defaults to NewFaultEngine(1, nil) if nil.
	Faults *FaultEngine
	// Limiter is the rate limiter; defaults to NewRateLimiter(0) if nil.
	Limiter *RateLimiter
	// Users is an optional auth map of username to password. If empty, auth is not required.
	Users map[string]string
	// Conn is the initial connection-level fault state; adjustable at runtime via SetConnSettings.
	Conn ConnSettings
}

// ReleaseSource is one release served by the same Server: its directory and manifest.
type ReleaseSource struct {
	Dir      string
	Manifest *ReleaseManifest
}

// ConnSettings are the connection-level faults, adjustable at runtime through POST /conn.
type ConnSettings struct {
	// MaxConnections caps simultaneous client connections; 0 means unlimited. A connection over
	// the cap gets "502 too many connections" and is closed.
	MaxConnections int `json:"max_connections"`
	// Refuse accepts then immediately closes without a greeting.
	Refuse bool `json:"refuse"`
	// RejectGreeting sends "400 service temporarily unavailable" and closes.
	RejectGreeting bool `json:"reject_greeting"`
	// AuthFail makes AUTHINFO PASS answer 481 and makes article commands require authentication.
	AuthFail bool `json:"auth_fail"`
}

// Snapshot captures a point-in-time state of served clean articles, article requests, and recorded IDs.
type Snapshot struct {
	Name        string         `json:"name"`
	CreatedAt   time.Time      `json:"created_at"`
	ServedClean map[string]int `json:"served_clean"`
	Requests    map[string]int `json:"requests"`
	RecordedIDs []string       `json:"recorded_ids,omitempty"`
}

// ServerStats reports aggregate counters, active rules, and snapshots for telemetry and assertions.
type ServerStats struct {
	ServedCleanTotal int64               `json:"served_clean_total"`
	FaultsFiredTotal int64               `json:"faults_fired_total"`
	BytesSentTotal   int64               `json:"bytes_sent_total"`
	RateMBps         float64             `json:"rate_mbps"`
	Conn             ConnSettings        `json:"conn"`
	ActiveConns      int                 `json:"active_connections"`
	PeakConns        int                 `json:"peak_connections"`
	ConnsRejected    int64               `json:"connections_rejected_total"`
	ServedClean      map[string]int      `json:"served_clean"`
	Requests         map[string]int      `json:"requests"`
	Rules            []FaultRule         `json:"rules"`
	Snapshots        map[string]Snapshot `json:"snapshots,omitempty"`
}

type segmentLoc struct {
	// Release indexes Server.releases: which release dir and pack this segment lives in.
	Release     int
	MessageID   string
	DiskName    string
	NZBFilename string
	Role        FileRole
	PartNum     int
	TotalParts  int
	Offset      int64
	Length      int
	TotalSize   int64
	CRC32       uint32
	WireOffset  int64
	WireLength  int
}

// Server is a disk-backed NNTP server for end-to-end stress and crash recovery testing.
type Server struct {
	cfg      ServerConfig
	segments map[string]segmentLoc
	faults   *FaultEngine
	limiter  *RateLimiter
	releases []serverRelease

	ln        net.Listener
	addr      string
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	servedCleanTotal atomic.Int64
	faultsFiredTotal atomic.Int64
	bytesSentTotal   atomic.Int64

	connMu       sync.Mutex
	connSettings ConnSettings
	openConns    map[net.Conn]struct{}
	peakConns    int
	connRejected int64

	statsMu     sync.Mutex
	servedClean map[string]int
	requests    map[string]int
	snapshots   map[string]Snapshot
}

// serverRelease is the per-release state the serving paths need: its directory and open pack.
type serverRelease struct {
	dir      string
	packFile *os.File
}

// NewServer initializes a disk-backed NNTP server with immutable pre-indexed segment locations.
func NewServer(cfg ServerConfig) (*Server, error) {
	if len(cfg.Releases) == 0 {
		return nil, errors.New("at least one release is required")
	}
	if cfg.Faults == nil {
		cfg.Faults = NewFaultEngine(1, nil)
	}
	if cfg.Limiter == nil {
		cfg.Limiter = NewRateLimiter(0)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}

	sources := cfg.Releases
	segments := make(map[string]segmentLoc)
	releases := make([]serverRelease, 0, len(sources))
	closePacks := func() {
		for _, r := range releases {
			if r.packFile != nil {
				_ = r.packFile.Close() //nolint:errcheck // best-effort close on a failed start
			}
		}
	}
	for i, src := range sources {
		if src.Manifest == nil || src.Dir == "" {
			closePacks()
			return nil, fmt.Errorf("release %d: directory and manifest are required", i)
		}
		for _, f := range src.Manifest.Files {
			totalParts := len(f.Segments)
			for _, seg := range f.Segments {
				cleanID := extractMessageID(seg.MessageID)
				if prev, dup := segments[cleanID]; dup {
					closePacks()
					return nil, fmt.Errorf("duplicate message-id %q in releases %q and %q", cleanID, sources[prev.Release].Dir, src.Dir)
				}
				segments[cleanID] = segmentLoc{
					Release:     i,
					MessageID:   cleanID,
					DiskName:    f.DiskName,
					NZBFilename: f.NZBFilename,
					Role:        f.Role,
					PartNum:     seg.PartNum,
					TotalParts:  totalParts,
					Offset:      seg.Offset,
					Length:      seg.Length,
					TotalSize:   f.TotalSize,
					CRC32:       seg.CRC32,
					WireOffset:  seg.WireOffset,
					WireLength:  seg.WireLength,
				}
			}
		}

		rel := serverRelease{dir: src.Dir}
		packPath := filepath.Join(src.Dir, "articles.pack")
		if f, err := os.Open(packPath); err == nil { //nolint:gosec // packPath is within the release dir
			rel.packFile = f
		}
		releases = append(releases, rel)
	}

	return &Server{
		cfg:          cfg,
		segments:     segments,
		faults:       cfg.Faults,
		limiter:      cfg.Limiter,
		releases:     releases,
		done:         make(chan struct{}),
		connSettings: cfg.Conn,
		openConns:    make(map[net.Conn]struct{}),
		servedClean:  make(map[string]int),
		requests:     make(map[string]int),
		snapshots:    make(map[string]Snapshot),
	}, nil
}

// Start binds the TCP listener and spawns the connection accept loop.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.ln = ln
	s.addr = ln.Addr().String()

	s.wg.Go(s.acceptLoop)

	return nil
}

// Addr returns the listening host:port address of the server.
func (s *Server) Addr() string {
	return s.addr
}

// Close gracefully stops accepting new connections, signals termination, and closes the listener.
func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.done)
		if s.ln != nil {
			closeErr = s.ln.Close()
		}
		s.wg.Wait()
		for _, r := range s.releases {
			if r.packFile != nil {
				_ = r.packFile.Close() //nolint:errcheck // best-effort packfile close
			}
		}
	})
	return closeErr
}

// Faults returns the fault engine driving article faults.
func (s *Server) Faults() *FaultEngine {
	return s.faults
}

// Limiter returns the rate limiter controlling transmission speeds.
func (s *Server) Limiter() *RateLimiter {
	return s.limiter
}

// Stats returns a copy of server operational statistics and delivery metrics.
func (s *Server) Stats() ServerStats {
	s.statsMu.Lock()
	scClone := maps.Clone(s.servedClean)
	reqClone := maps.Clone(s.requests)
	snapsClone := make(map[string]Snapshot, len(s.snapshots))
	for k, v := range s.snapshots {
		snapsClone[k] = cloneSnapshot(v)
	}
	s.statsMu.Unlock()

	s.connMu.Lock()
	conn, active, peak, rejected := s.connSettings, len(s.openConns), s.peakConns, s.connRejected
	s.connMu.Unlock()

	return ServerStats{
		ServedCleanTotal: s.servedCleanTotal.Load(),
		FaultsFiredTotal: s.faultsFiredTotal.Load(),
		BytesSentTotal:   s.bytesSentTotal.Load(),
		RateMBps:         s.limiter.RateMBps(),
		Conn:             conn,
		ActiveConns:      active,
		PeakConns:        peak,
		ConnsRejected:    rejected,
		ServedClean:      scClone,
		Requests:         reqClone,
		Rules:            s.faults.Rules(),
		Snapshots:        snapsClone,
	}
}

// SetConnSettings replaces the connection-level fault state; it applies to later connections
// and, for AuthFail, to commands on connections already open.
func (s *Server) SetConnSettings(c ConnSettings) {
	s.connMu.Lock()
	s.connSettings = c
	s.connMu.Unlock()
}

// ConnSettings returns the current connection-level fault state.
func (s *Server) ConnSettings() ConnSettings {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.connSettings
}

// KillActive abruptly closes every open client connection and returns how many it closed.
func (s *Server) KillActive() int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for c := range s.openConns {
		_ = c.Close() //nolint:errcheck // abrupt close is the point
	}
	return len(s.openConns)
}

// admit applies the connection-level faults to a freshly accepted connection. It returns
// the greeting line to send and whether the connection is admitted (tracked in openConns).
// A non-admitted connection is closed by the caller after the greeting, if any.
func (s *Server) admit(c net.Conn) (greeting string, ok bool) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	cs := s.connSettings
	switch {
	case cs.Refuse:
		s.connRejected++
		return "", false
	case cs.RejectGreeting:
		s.connRejected++
		return "400 service temporarily unavailable", false
	case cs.MaxConnections > 0 && len(s.openConns) >= cs.MaxConnections:
		s.connRejected++
		return "502 too many connections", false
	}
	s.openConns[c] = struct{}{}
	s.peakConns = max(s.peakConns, len(s.openConns))
	return "200 localnntp ready", true
}

func (s *Server) release(c net.Conn) {
	s.connMu.Lock()
	delete(s.openConns, c)
	s.connMu.Unlock()
}

// authRequired reports whether article commands must be preceded by authentication.
func (s *Server) authRequired() bool {
	return len(s.cfg.Users) > 0 || s.ConnSettings().AuthFail
}

// TakeSnapshot records an immutable snapshot of currently served articles along with recorded IDs.
func (s *Server) TakeSnapshot(name string, recordedIDs []string) Snapshot {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()

	snap := Snapshot{
		Name:        name,
		CreatedAt:   time.Now().UTC(),
		ServedClean: maps.Clone(s.servedClean),
		Requests:    maps.Clone(s.requests),
		RecordedIDs: slices.Clone(recordedIDs),
	}
	s.snapshots[name] = snap
	return cloneSnapshot(snap)
}

// GetSnapshot retrieves a deep copy of a named snapshot if present.
func (s *Server) GetSnapshot(name string) (Snapshot, bool) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()

	snap, ok := s.snapshots[name]
	if !ok {
		return Snapshot{}, false
	}
	return cloneSnapshot(snap), true
}

func cloneSnapshot(s Snapshot) Snapshot {
	return Snapshot{
		Name:        s.Name,
		CreatedAt:   s.CreatedAt,
		ServedClean: maps.Clone(s.ServedClean),
		Requests:    maps.Clone(s.Requests),
		RecordedIDs: slices.Clone(s.RecordedIDs),
	}
}

func (s *Server) recordServedClean(id string) {
	s.statsMu.Lock()
	s.servedClean[id]++
	s.statsMu.Unlock()
}

func (s *Server) recordRequest(id string) {
	s.statsMu.Lock()
	s.requests[id]++
	s.statsMu.Unlock()
}

func (s *Server) waitOnAcceptError(err error, backoff *time.Duration) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
	}
	if *backoff <= 0 {
		*backoff = 5 * time.Millisecond
	} else {
		*backoff = min(*backoff*2, time.Second)
	}
	slog.Warn("localnntp accept error, retrying", "err", err, "backoff", *backoff)
	t := time.NewTimer(*backoff)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.done:
		return false
	}
}

func (s *Server) acceptLoop() {
	var backoff time.Duration
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if !s.waitOnAcceptError(err, &backoff) {
				return
			}
			continue
		}
		backoff = 0
		s.wg.Go(func() {
			s.serveConn(c)
		})
	}
}

type connState struct {
	authed      bool
	pendingUser string
}

func (s *Server) serveConn(c net.Conn) {
	defer func() { _ = c.Close() }() //nolint:errcheck // best-effort close on connection termination

	connCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		select {
		case <-s.done:
			cancel()
			_ = c.Close() //nolint:errcheck // best-effort close on server shutdown
		case <-connCtx.Done():
		}
	}()

	bw := bufio.NewWriter(c)
	br := bufio.NewReader(c)
	cs := &connState{}
	var wireBuf []byte

	greeting, admitted := s.admit(c)
	if admitted {
		defer s.release(c)
	}
	if greeting != "" {
		if err := s.send(bw, greeting); err != nil {
			return
		}
	}
	if !admitted {
		return
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		if cmd == "" {
			continue
		}

		parts := strings.SplitN(cmd, " ", 3)
		verb := strings.ToUpper(parts[0])

		switch verb {
		case "CAPABILITIES":
			caps := []string{
				"101 capability list follows",
				"VERSION 2",
				"READER",
				"BODY",
				"STAT",
				".",
			}
			for _, capLine := range caps {
				if _, err := fmt.Fprintf(bw, "%s\r\n", capLine); err != nil {
					return
				}
			}
			if err := bw.Flush(); err != nil {
				return
			}

		case "GROUP":
			groupName := ""
			if len(parts) > 1 {
				groupName = parts[1]
			}
			if err := s.send(bw, fmt.Sprintf("211 1 1 1 %s", groupName)); err != nil {
				return
			}

		case "AUTHINFO":
			if len(parts) < 3 {
				if err := s.send(bw, "500 command not recognized"); err != nil {
					return
				}
				continue
			}
			subCmd := strings.ToUpper(parts[1])
			val := parts[2]
			switch subCmd {
			case "USER":
				if len(s.cfg.Users) > 0 {
					if _, ok := s.cfg.Users[val]; !ok {
						if err := s.send(bw, "481 authentication failed"); err != nil {
							return
						}
						continue
					}
				}
				cs.pendingUser = val
				if err := s.send(bw, "381 password required"); err != nil {
					return
				}
			case "PASS":
				if s.ConnSettings().AuthFail {
					if err := s.send(bw, "481 authentication failed"); err != nil {
						return
					}
					continue
				}
				if len(s.cfg.Users) > 0 {
					expected, ok := s.cfg.Users[cs.pendingUser]
					if !ok || expected != val {
						if err := s.send(bw, "481 authentication failed"); err != nil {
							return
						}
						continue
					}
				}
				cs.authed = true
				if err := s.send(bw, "281 authentication accepted"); err != nil {
					return
				}
			default:
				if err := s.send(bw, "500 command not recognized"); err != nil {
					return
				}
			}

		case "STAT":
			if len(parts) < 2 {
				if err := s.send(bw, "500 command not recognized"); err != nil {
					return
				}
				continue
			}
			if !s.handleStat(bw, cs, parts[1]) {
				return
			}

		case "BODY":
			if len(parts) < 2 {
				if err := s.send(bw, "500 command not recognized"); err != nil {
					return
				}
				continue
			}
			if !s.handleBodyOrArticle(connCtx, bw, cs, parts[1], false, &wireBuf) {
				return
			}

		case "ARTICLE":
			if len(parts) < 2 {
				if err := s.send(bw, "500 command not recognized"); err != nil {
					return
				}
				continue
			}
			if !s.handleBodyOrArticle(connCtx, bw, cs, parts[1], true, &wireBuf) {
				return
			}

		case "QUIT":
			_ = s.send(bw, "205 goodbye") //nolint:errcheck // closing connection immediately after
			return

		default:
			if err := s.send(bw, "500 command not recognized"); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleStat(bw *bufio.Writer, cs *connState, rawID string) bool {
	if s.authRequired() && !cs.authed {
		return s.send(bw, "480 authentication required") == nil
	}
	cleanID := extractMessageID(rawID)
	loc, ok := s.segments[cleanID]
	if !ok {
		return s.send(bw, "430 no article with that message-id") == nil
	}

	target := ArticleTarget{
		MessageID:   cleanID,
		DiskName:    loc.DiskName,
		NZBFilename: loc.NZBFilename,
		Role:        loc.Role,
		PartNum:     loc.PartNum,
	}

	if _, hasFault := s.faults.MatchAction(target, FaultDrop); hasFault {
		s.faultsFiredTotal.Add(1)
		return s.send(bw, "430 no article with that message-id") == nil
	}

	return s.send(bw, fmt.Sprintf("223 0 <%s>", cleanID)) == nil
}

func writeStatusAndHeaders(bw *bufio.Writer, cleanID string, isArticle bool) error {
	statusLine := fmt.Sprintf("222 0 <%s> body follows\r\n", cleanID)
	if isArticle {
		statusLine = fmt.Sprintf("220 0 <%s> article follows\r\nMessage-ID: <%s>\r\n\r\n", cleanID, cleanID)
	}
	if _, err := bw.WriteString(statusLine); err != nil {
		return err
	}
	return bw.Flush()
}

func (s *Server) writeRateLimited(ctx context.Context, bw *bufio.Writer, payload []byte) (int, error) {
	const chunkSize = 32 * 1024
	total := 0
	for len(payload) > 0 {
		chunk := payload[:min(len(payload), chunkSize)]
		if err := s.limiter.Wait(ctx, len(chunk)); err != nil {
			return total, err
		}
		n, err := bw.Write(chunk)
		total += n
		if err != nil {
			return total, err
		}
		payload = payload[len(chunk):]
	}
	if err := bw.Flush(); err != nil {
		return total, err
	}
	return total, nil
}

func (s *Server) handleBodyOrArticle(ctx context.Context, bw *bufio.Writer, cs *connState, rawID string, isArticle bool, wireBuf *[]byte) bool {
	if s.authRequired() && !cs.authed {
		return s.send(bw, "480 authentication required") == nil
	}
	cleanID := extractMessageID(rawID)
	loc, ok := s.segments[cleanID]
	if !ok {
		return s.send(bw, "430 no article with that message-id") == nil
	}
	s.recordRequest(cleanID)

	target := ArticleTarget{
		MessageID:   cleanID,
		DiskName:    loc.DiskName,
		NZBFilename: loc.NZBFilename,
		Role:        loc.Role,
		PartNum:     loc.PartNum,
	}

	rule, hasFault := s.faults.Match(target)
	if hasFault && rule.Action == FaultDrop {
		s.faultsFiredTotal.Add(1)
		return s.send(bw, "430 no article with that message-id") == nil
	}

	if hasFault && rule.Action == FaultStall {
		s.faultsFiredTotal.Add(1)
		if err := writeStatusAndHeaders(bw, cleanID, isArticle); err != nil {
			return false
		}
		timer := time.NewTimer(rule.TimeoutAfter)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.done:
		case <-ctx.Done():
		}
		return false
	}

	if !hasFault && s.releases[loc.Release].packFile != nil && loc.WireLength > 0 {
		return s.serveFastPath(ctx, bw, cleanID, loc, isArticle, wireBuf)
	}

	return s.serveSlowOrFaultPath(ctx, bw, cleanID, loc, isArticle, hasFault, rule)
}

func (s *Server) serveFastPath(ctx context.Context, bw *bufio.Writer, cleanID string, loc segmentLoc, isArticle bool, wireBuf *[]byte) bool {
	if cap(*wireBuf) < loc.WireLength {
		*wireBuf = make([]byte, loc.WireLength)
	} else {
		*wireBuf = (*wireBuf)[:loc.WireLength]
	}
	buf := *wireBuf
	if _, err := s.releases[loc.Release].packFile.ReadAt(buf, loc.WireOffset); err != nil {
		return s.send(bw, "430 no article with that message-id") == nil
	}

	if err := writeStatusAndHeaders(bw, cleanID, isArticle); err != nil {
		return false
	}
	sent, err := s.writeRateLimited(ctx, bw, buf)
	s.bytesSentTotal.Add(int64(sent))
	if err != nil {
		return false
	}

	s.servedCleanTotal.Add(1)
	s.recordServedClean(cleanID)
	return true
}

func (s *Server) serveSlowOrFaultPath(ctx context.Context, bw *bufio.Writer, cleanID string, loc segmentLoc, isArticle, hasFault bool, rule FaultRule) bool {
	filePath := filepath.Join(s.releases[loc.Release].dir, "files", loc.DiskName)
	f, err := os.Open(filePath) //nolint:gosec // path is operator-configured release dir
	if err != nil {
		return s.send(bw, "430 no article with that message-id") == nil
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort file close

	raw := make([]byte, loc.Length)
	if _, err := io.ReadFull(io.NewSectionReader(f, loc.Offset, int64(loc.Length)), raw); err != nil {
		return s.send(bw, "430 no article with that message-id") == nil
	}

	if hasFault && rule.Action == FaultCorruptPayload {
		raw = CorruptPayloadSlice(raw, rule.CorruptBytes)
	}

	var body []byte
	if loc.TotalParts == 1 {
		body = mocknntp.EncodeYEnc(loc.NZBFilename, raw)
	} else {
		body = mocknntp.EncodeYEncPart(loc.NZBFilename, loc.PartNum, loc.TotalParts, loc.TotalSize, loc.Offset, raw)
	}

	if hasFault && rule.Action == FaultCorruptWire {
		body = CorruptWireBody(body, rule.CorruptBytes)
	}

	if hasFault && rule.Action == FaultDropMidBody {
		s.faultsFiredTotal.Add(1)
		if err := writeStatusAndHeaders(bw, cleanID, isArticle); err != nil {
			return false
		}
		half := body[:len(body)/2]
		sent, _ := s.writeRateLimited(ctx, bw, half) //nolint:errcheck // closing mid-body intentionally
		_ = bw.Flush()                               //nolint:errcheck // best-effort flush before closing connection
		s.bytesSentTotal.Add(int64(sent))
		return false
	}

	if err := writeStatusAndHeaders(bw, cleanID, isArticle); err != nil {
		return false
	}
	wireBytes := FormatDotStuffedWire(body)
	sent, err := s.writeRateLimited(ctx, bw, wireBytes)
	s.bytesSentTotal.Add(int64(sent))
	if err != nil {
		return false
	}

	if hasFault && (rule.Action == FaultCorruptPayload || rule.Action == FaultCorruptWire) {
		s.faultsFiredTotal.Add(1)
	} else if !hasFault {
		s.servedCleanTotal.Add(1)
		s.recordServedClean(cleanID)
	}

	return true
}

func (s *Server) send(bw *bufio.Writer, line string) error {
	if _, err := fmt.Fprintf(bw, "%s\r\n", line); err != nil {
		return err
	}
	return bw.Flush()
}

func extractMessageID(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") && len(s) >= 2 {
		s = s[1 : len(s)-1]
	}
	return strings.TrimSpace(s)
}
