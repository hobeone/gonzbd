// Package mocknntp provides a configurable in-process NNTP server for
// integration tests. It speaks the subset of NNTP required by the
// internal/nntp client: greeting, CAPABILITIES, AUTHINFO, BODY, STAT, QUIT.
package mocknntp

import (
	"bufio"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"
)

// Config controls server behavior. A zero-value Config is valid and starts an
// open server that accepts BODY and STAT for any registered article.
type Config struct {
	// RequireAuth requires AUTHINFO USER/PASS before BODY/STAT work.
	// When set, Users must be non-empty.
	RequireAuth bool
	Users       map[string]string // username -> password

	// DisableBody makes BODY return 500 "command not recognized",
	// forcing the client onto STAT+ARTICLE fallbacks.
	DisableBody bool

	// DisableStat similarly disables STAT.
	DisableStat bool

	// FailureRate, if > 0 and < 1, causes each BODY/STAT to randomly
	// return 430 with that probability. The server seeds its own RNG.
	FailureRate float64

	// ResponseDelay is added before each command's response. Useful for
	// exercising timeout/cancellation paths.
	ResponseDelay time.Duration

	// Greeting overrides the default "200 mocknntp ready". Use "201 ..."
	// to simulate a no-posting server.
	Greeting string

	// Logger receives per-connection debug lines. Defaults to a discard
	// logger when nil.
	Logger *slog.Logger
}

// Server is a mock NNTP endpoint for tests. It listens on a local TCP port
// and responds to a declarative set of registered articles and credentials.
//
// Typical use:
//
//	srv := mocknntp.NewServer(mocknntp.Config{RequireAuth: true, Users: map[string]string{"u": "p"}})
//	srv.AddArticle("abc@host", mocknntp.EncodeYEnc("file.bin", payload))
//	if err := srv.Start(); err != nil { ... }
//	t.Cleanup(func() { _ = srv.Close() })
type Server struct {
	cfg      Config
	log      *slog.Logger
	articles map[string][]byte // messageID -> raw body (not dot-stuffed; we stuff on send)
	mu       sync.RWMutex

	ln        net.Listener
	addr      string
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewServer builds an unstarted Server. Register articles with AddArticle
// before calling Start.
func NewServer(cfg Config) *Server {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{
		cfg:      cfg,
		log:      log,
		articles: make(map[string][]byte),
		done:     make(chan struct{}),
	}
}

// AddArticle registers a raw article body to serve for the given Message-ID
// (without angle brackets). body is sent verbatim after the "222 ..." response
// line; the server handles dot-stuffing and the terminating ".\r\n" itself.
//
// AddArticle is safe to call before Start or concurrently from a test goroutine
// while the server is running.
func (s *Server) AddArticle(messageID string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.articles[messageID] = body
}

// Start begins listening on 127.0.0.1:0 and returns when the listener is
// ready. Call Addr() to discover the ephemeral port.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("mocknntp: listen: %w", err)
	}
	s.ln = ln
	s.addr = ln.Addr().String()
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// Addr returns the host:port the server is listening on. Valid only after
// Start returns nil.
func (s *Server) Addr() string { return s.addr }

// Close shuts down the listener and waits for active connections to drain.
// Safe to call multiple times.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.ln.Close()
	})
	s.wg.Wait()
	return err
}

// acceptLoop runs in a goroutine and accepts connections until the listener
// is closed. Each connection is handled in its own goroutine tracked by wg.
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			// Accept errors when the listener is closed; that is the
			// expected shutdown path signalled by Close.
			select {
			case <-s.done:
				return
			default:
				s.log.Error("mocknntp: accept", "err", err)
				return
			}
		}
		s.wg.Add(1)
		go s.serveConn(c)
	}
}

// connState holds per-connection mutable state for the NNTP state machine.
type connState struct {
	authed      bool   // user has completed AUTHINFO successfully
	pendingUser string // username from AUTHINFO USER, awaiting PASS
}

// serveConn drives the NNTP state machine for a single client connection.
// It exits when the client disconnects, sends QUIT, or when done is closed.
func (s *Server) serveConn(c net.Conn) {
	defer s.wg.Done()
	defer func() { _ = c.Close() }() //nolint:errcheck // read-only cleanup path

	// Respect server Close: when done closes, the Read on the next command
	// returns an error via the closed net.Conn, which exits the loop.
	go func() {
		<-s.done
		_ = c.Close() //nolint:errcheck // closing connection on server shutdown
	}()

	bw := bufio.NewWriter(c)
	br := bufio.NewReader(c)
	cs := &connState{}

	greeting := s.cfg.Greeting
	if greeting == "" {
		greeting = "200 mocknntp ready"
	}
	if err := s.send(bw, greeting); err != nil {
		return
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// EOF or closed connection — normal exit.
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		s.log.Debug("mocknntp: recv", "cmd", cmd)

		if s.cfg.ResponseDelay > 0 {
			// Wait unless the server is shutting down.
			select {
			case <-s.done:
				return
			case <-time.After(s.cfg.ResponseDelay):
			}
		}

		upper := strings.ToUpper(cmd)
		switch {
		case upper == "CAPABILITIES":
			s.handleCapabilities(bw)
		case strings.HasPrefix(upper, "AUTHINFO USER "):
			if !s.handleAuthUser(bw, cs, cmd[len("AUTHINFO USER "):]) {
				return
			}
		case strings.HasPrefix(upper, "AUTHINFO PASS "):
			if !s.handleAuthPass(bw, cs, cmd[len("AUTHINFO PASS "):]) {
				return
			}
		case strings.HasPrefix(upper, "BODY "):
			id := extractMessageID(cmd[len("BODY "):])
			s.handleBody(bw, cs, id)
		case strings.HasPrefix(upper, "STAT "):
			id := extractMessageID(cmd[len("STAT "):])
			s.handleStat(bw, cs, id)
		case upper == "QUIT":
			_ = s.send(bw, "205 goodbye") //nolint:errcheck // closing anyway
			return
		default:
			_ = s.send(bw, "500 command not recognized") //nolint:errcheck // best-effort
		}
	}
}

// handleCapabilities writes the CAPABILITIES response.
func (s *Server) handleCapabilities(bw *bufio.Writer) {
	_ = s.send(bw, "101 capability list follows") //nolint:errcheck // best-effort
	_ = s.writeLine(bw, "VERSION 2")              //nolint:errcheck // best-effort
	_ = s.writeLine(bw, "READER")                 //nolint:errcheck // best-effort
	if !s.cfg.DisableBody {
		_ = s.writeLine(bw, "BODY") //nolint:errcheck // best-effort
	}
	if !s.cfg.DisableStat {
		_ = s.writeLine(bw, "STAT") //nolint:errcheck // best-effort
	}
	_ = s.writeLine(bw, ".") //nolint:errcheck // best-effort
	_ = bw.Flush()           //nolint:errcheck // best-effort
}

// handleAuthUser processes an AUTHINFO USER command.
// Returns false if the connection should be terminated (write error).
func (s *Server) handleAuthUser(bw *bufio.Writer, cs *connState, user string) bool {
	if _, ok := s.cfg.Users[user]; !ok && s.cfg.RequireAuth {
		// Unknown user — reject immediately rather than prompting for a
		// password that will also fail, matching typical server behaviour.
		_ = s.send(bw, "481 authentication failed") //nolint:errcheck // closing
		return false
	}
	cs.pendingUser = user
	if err := s.send(bw, "381 password required"); err != nil {
		return false
	}
	return true
}

// handleAuthPass processes an AUTHINFO PASS command.
// Returns false if the connection should be terminated.
func (s *Server) handleAuthPass(bw *bufio.Writer, cs *connState, pass string) bool {
	expectedPass, userExists := s.cfg.Users[cs.pendingUser]
	if !userExists || expectedPass != pass {
		_ = s.send(bw, "481 authentication failed") //nolint:errcheck // closing
		return false
	}
	cs.authed = true
	cs.pendingUser = ""
	if err := s.send(bw, "281 authentication accepted"); err != nil {
		return false
	}
	return true
}

// handleBody processes a BODY <message-id> command.
func (s *Server) handleBody(bw *bufio.Writer, cs *connState, messageID string) {
	if s.cfg.DisableBody {
		_ = s.send(bw, "500 command not recognized") //nolint:errcheck // best-effort
		return
	}
	if s.cfg.RequireAuth && !cs.authed {
		_ = s.send(bw, "480 authentication required") //nolint:errcheck // best-effort
		return
	}
	if s.cfg.FailureRate > 0 && rand.Float64() < s.cfg.FailureRate { //nolint:gosec // rand is fine for test randomness
		_ = s.send(bw, "430 no article with that message-id") //nolint:errcheck // best-effort
		return
	}

	s.mu.RLock()
	body, ok := s.articles[messageID]
	s.mu.RUnlock()

	if !ok {
		_ = s.send(bw, "430 no article with that message-id") //nolint:errcheck // best-effort
		return
	}

	_ = s.send(bw, fmt.Sprintf("222 0 <%s> body follows", messageID)) //nolint:errcheck // best-effort
	_ = s.sendDotStuffed(bw, body)                                    //nolint:errcheck // best-effort
}

// handleStat processes a STAT <message-id> command.
func (s *Server) handleStat(bw *bufio.Writer, cs *connState, messageID string) {
	if s.cfg.DisableStat {
		_ = s.send(bw, "500 command not recognized") //nolint:errcheck // best-effort
		return
	}
	if s.cfg.RequireAuth && !cs.authed {
		_ = s.send(bw, "480 authentication required") //nolint:errcheck // best-effort
		return
	}
	if s.cfg.FailureRate > 0 && rand.Float64() < s.cfg.FailureRate { //nolint:gosec // rand is fine for test randomness
		_ = s.send(bw, "430 no article with that message-id") //nolint:errcheck // best-effort
		return
	}

	s.mu.RLock()
	_, ok := s.articles[messageID]
	s.mu.RUnlock()

	if !ok {
		_ = s.send(bw, "430 no article with that message-id") //nolint:errcheck // best-effort
		return
	}
	_ = s.send(bw, fmt.Sprintf("223 0 <%s>", messageID)) //nolint:errcheck // best-effort
}

// send writes a single CRLF-terminated line and flushes the writer.
func (s *Server) send(bw *bufio.Writer, line string) error {
	if _, err := fmt.Fprintf(bw, "%s\r\n", line); err != nil {
		return err
	}
	return bw.Flush()
}

// writeLine writes a single CRLF-terminated line without flushing. The caller
// must flush when the response is complete.
func (s *Server) writeLine(bw *bufio.Writer, line string) error {
	_, err := fmt.Fprintf(bw, "%s\r\n", line)
	return err
}

// sendDotStuffed writes a dot-stuffed body followed by the terminator ".\r\n"
// and flushes. Dot-stuffing: any line that begins with "." must have an extra
// "." prepended so the client can distinguish data from the terminator.
func (s *Server) sendDotStuffed(bw *bufio.Writer, body []byte) error {
	// Split body into lines. We preserve the original line endings from the
	// stored body and just transmit them verbatim, adding dot-stuffing where
	// needed. The body may contain LF-only or CRLF line endings; we normalise
	// to CRLF for the wire.
	remaining := body
	for len(remaining) > 0 {
		// Find the next LF.
		idx := -1
		for i, b := range remaining {
			if b == '\n' {
				idx = i
				break
			}
		}

		var lineBytes []byte
		if idx < 0 {
			// No more newlines — treat the remainder as the last line.
			lineBytes = remaining
			remaining = nil
		} else {
			lineBytes = remaining[:idx]
			remaining = remaining[idx+1:]
			// Strip trailing CR if the body uses CRLF endings.
			if len(lineBytes) > 0 && lineBytes[len(lineBytes)-1] == '\r' {
				lineBytes = lineBytes[:len(lineBytes)-1]
			}
		}

		// Dot-stuffing: the NNTP protocol reserves "." at line start as the body
		// terminator (RFC 3977 §3.1.1); any real line beginning with "." must be
		// transmitted as ".." and un-stuffed on receipt.
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

	// Terminator.
	if _, err := bw.WriteString(".\r\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// extractMessageID strips optional angle brackets from a message-ID as
// received on the wire (e.g. "<abc@host>" → "abc@host", "abc@host" → "abc@host").
func extractMessageID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>' {
		return s[1 : len(s)-1]
	}
	return s
}
