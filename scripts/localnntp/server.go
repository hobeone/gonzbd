// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	// ReleaseDir is the directory containing `files/<DiskName>`.
	ReleaseDir string
	// Manifest is the metadata describing the release structure and segments.
	Manifest *ReleaseManifest
	// ListenAddr is the TCP address to bind, defaults to "127.0.0.1:0" if empty.
	ListenAddr string
	// Faults is the fault injection engine; defaults to NewFaultEngine(1, nil) if nil.
	Faults *FaultEngine
	// Limiter is the rate limiter; defaults to NewRateLimiter(0) if nil.
	Limiter *RateLimiter
	// Users is an optional auth map of username to password. If empty, auth is not required.
	Users map[string]string
}

// Snapshot captures a point-in-time state of served clean articles and recorded IDs.
type Snapshot struct {
	Name        string         `json:"name"`
	CreatedAt   time.Time      `json:"created_at"`
	ServedClean map[string]int `json:"served_clean"`
	RecordedIDs []string       `json:"recorded_ids,omitempty"`
}

// ServerStats reports aggregate counters, active rules, and snapshots for telemetry and assertions.
type ServerStats struct {
	ServedCleanTotal int64               `json:"served_clean_total"`
	FaultsFiredTotal int64               `json:"faults_fired_total"`
	BytesSentTotal   int64               `json:"bytes_sent_total"`
	RateMBps         float64             `json:"rate_mbps"`
	ServedClean      map[string]int      `json:"served_clean"`
	Rules            []FaultRule         `json:"rules"`
	Snapshots        map[string]Snapshot `json:"snapshots,omitempty"`
}

type segmentLoc struct {
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
	packFile *os.File

	ln        net.Listener
	addr      string
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	servedCleanTotal atomic.Int64
	faultsFiredTotal atomic.Int64
	bytesSentTotal   atomic.Int64

	statsMu     sync.Mutex
	servedClean map[string]int
	snapshots   map[string]Snapshot
}

// NewServer initializes a disk-backed NNTP server with immutable pre-indexed segment locations.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Manifest == nil {
		return nil, errors.New("manifest is required")
	}
	if cfg.ReleaseDir == "" {
		return nil, errors.New("release directory is required")
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

	segments := make(map[string]segmentLoc)
	for _, f := range cfg.Manifest.Files {
		totalParts := len(f.Segments)
		for _, seg := range f.Segments {
			cleanID := extractMessageID(seg.MessageID)
			segments[cleanID] = segmentLoc{
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

	var packFile *os.File
	packPath := filepath.Join(cfg.ReleaseDir, "articles.pack")
	if f, err := os.Open(packPath); err == nil { //nolint:gosec // packPath is within ReleaseDir
		packFile = f
	}

	return &Server{
		cfg:         cfg,
		segments:    segments,
		faults:      cfg.Faults,
		limiter:     cfg.Limiter,
		packFile:    packFile,
		done:        make(chan struct{}),
		servedClean: make(map[string]int),
		snapshots:   make(map[string]Snapshot),
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
		if s.packFile != nil {
			_ = s.packFile.Close() //nolint:errcheck // best-effort packfile close
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
	snapsClone := make(map[string]Snapshot, len(s.snapshots))
	for k, v := range s.snapshots {
		snapsClone[k] = cloneSnapshot(v)
	}
	s.statsMu.Unlock()

	return ServerStats{
		ServedCleanTotal: s.servedCleanTotal.Load(),
		FaultsFiredTotal: s.faultsFiredTotal.Load(),
		BytesSentTotal:   s.bytesSentTotal.Load(),
		RateMBps:         s.limiter.RateMBps(),
		ServedClean:      scClone,
		Rules:            s.faults.Rules(),
		Snapshots:        snapsClone,
	}
}

// TakeSnapshot records an immutable snapshot of currently served articles along with recorded IDs.
func (s *Server) TakeSnapshot(name string, recordedIDs []string) Snapshot {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()

	scClone := maps.Clone(s.servedClean)

	snap := Snapshot{
		Name:        name,
		CreatedAt:   time.Now().UTC(),
		ServedClean: scClone,
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
		RecordedIDs: slices.Clone(s.RecordedIDs),
	}
}

func (s *Server) recordServedClean(id string) {
	s.statsMu.Lock()
	s.servedClean[id]++
	s.statsMu.Unlock()
}

func (s *Server) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				return
			}
		}
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

	if err := s.send(bw, "200 localnntp ready"); err != nil {
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
	if len(s.cfg.Users) > 0 && !cs.authed {
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

	rule, hasFault := s.faults.Match(target)
	if hasFault && rule.Action == FaultDrop {
		s.faultsFiredTotal.Add(1)
		return s.send(bw, "430 no article with that message-id") == nil
	}

	return s.send(bw, fmt.Sprintf("223 0 <%s>", cleanID)) == nil
}

func (s *Server) handleBodyOrArticle(ctx context.Context, bw *bufio.Writer, cs *connState, rawID string, isArticle bool, wireBuf *[]byte) bool {
	if len(s.cfg.Users) > 0 && !cs.authed {
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

	rule, hasFault := s.faults.Match(target)
	if hasFault && rule.Action == FaultDrop {
		s.faultsFiredTotal.Add(1)
		return s.send(bw, "430 no article with that message-id") == nil
	}

	if hasFault && rule.Action == FaultStall {
		s.faultsFiredTotal.Add(1)
		statusLine := fmt.Sprintf("222 0 <%s> body follows\r\n", cleanID)
		if isArticle {
			statusLine = fmt.Sprintf("220 0 <%s> article follows\r\n", cleanID)
		}
		if _, err := bw.WriteString(statusLine); err != nil {
			return false
		}
		if err := bw.Flush(); err != nil {
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

	// Fast path: pre-encoded wire packfile when no fault is matched.
	if !hasFault && s.packFile != nil && loc.WireLength > 0 {
		if cap(*wireBuf) < loc.WireLength {
			*wireBuf = make([]byte, loc.WireLength)
		} else {
			*wireBuf = (*wireBuf)[:loc.WireLength]
		}
		buf := *wireBuf
		if _, err := s.packFile.ReadAt(buf, loc.WireOffset); err != nil {
			return s.send(bw, "430 no article with that message-id") == nil
		}

		if err := s.limiter.Wait(ctx, loc.WireLength); err != nil {
			return false
		}

		statusLine := fmt.Sprintf("222 0 <%s> body follows\r\n", cleanID)
		if isArticle {
			statusLine = fmt.Sprintf("220 0 <%s> article follows\r\n", cleanID)
		}
		if _, err := bw.WriteString(statusLine); err != nil {
			return false
		}
		if _, err := bw.Write(buf); err != nil {
			return false
		}
		if err := bw.Flush(); err != nil {
			return false
		}

		s.bytesSentTotal.Add(int64(loc.WireLength))
		s.servedCleanTotal.Add(1)
		s.recordServedClean(cleanID)
		return true
	}

	filePath := filepath.Join(s.cfg.ReleaseDir, "files", loc.DiskName)
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

	if err := s.limiter.Wait(ctx, len(body)); err != nil {
		return false
	}

	if hasFault && rule.Action == FaultDropMidBody {
		s.faultsFiredTotal.Add(1)
		statusLine := fmt.Sprintf("222 0 <%s> body follows\r\n", cleanID)
		if isArticle {
			statusLine = fmt.Sprintf("220 0 <%s> article follows\r\n", cleanID)
		}
		if _, err := bw.WriteString(statusLine); err != nil {
			return false
		}
		half := body[:len(body)/2]
		if _, err := bw.Write(half); err != nil {
			return false
		}
		_ = bw.Flush() //nolint:errcheck // flushing partial payload before closing connection
		return false
	}

	statusLine := fmt.Sprintf("222 0 <%s> body follows\r\n", cleanID)
	if isArticle {
		statusLine = fmt.Sprintf("220 0 <%s> article follows\r\n", cleanID)
	}
	if _, err := bw.WriteString(statusLine); err != nil {
		return false
	}
	if err := sendDotStuffed(bw, body); err != nil {
		return false
	}

	s.bytesSentTotal.Add(int64(len(body)))
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

func sendDotStuffed(bw *bufio.Writer, body []byte) error {
	remaining := body
	for len(remaining) > 0 {
		idx := bytes.IndexByte(remaining, '\n')
		var lineBytes []byte
		if idx < 0 {
			lineBytes = remaining
			remaining = nil
		} else {
			lineBytes = remaining[:idx]
			remaining = remaining[idx+1:]
			if len(lineBytes) > 0 && lineBytes[len(lineBytes)-1] == '\r' {
				lineBytes = lineBytes[:len(lineBytes)-1]
			}
		}

		if len(lineBytes) > 0 && lineBytes[0] == '.' {
			if _, err := bw.WriteString("."); err != nil {
				return err
			}
		}
		if _, err := bw.Write(lineBytes); err != nil {
			return err
		}
		if _, err := bw.WriteString("\r\n"); err != nil {
			return err
		}
	}
	if _, err := bw.WriteString(".\r\n"); err != nil {
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
