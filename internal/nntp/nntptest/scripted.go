// Package nntptest provides a scripted fake NNTP server for end-to-end
// pipeline tests that want real socket semantics (dial, fetch, close)
// without talking to a Usenet provider.
//
// The server accepts unlimited concurrent connections, answers
// CAPABILITIES / AUTHINFO / BODY / ARTICLE / QUIT, and serves article
// bodies from an in-memory map keyed by Message-ID. Callers can inject
// per-article failures — not-found, mid-body connection drop, or an
// indefinite stall — to exercise dispatcher recovery paths.
//
// The fake is intentionally minimal: no GROUP handling, no overview,
// no compression, no pipelining negotiation. The downloader code paths
// the state-machine tests exercise don't need any of those.
package nntptest

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/config"
)

// FailureMode is a scripted failure applied on the next Fetch of a
// message-ID. Failures are one-shot: after firing, the entry is
// cleared and subsequent fetches succeed (if the article exists).
type FailureMode int

const (
	// FailureNone means no failure is scheduled. Passing this to
	// InjectFailure clears any prior entry.
	FailureNone FailureMode = iota
	// FailureNotFound returns 430. Connection stays usable.
	FailureNotFound
	// FailureDropMidBody writes a 222 response header and a short
	// prefix of the body, then closes the connection without the
	// terminating dot line.
	FailureDropMidBody
	// FailureStall writes a 222 response header and then blocks
	// indefinitely until the server is Closed or the client
	// disconnects. Useful for testing context-cancel paths.
	FailureStall
)

// Scripted is a running fake NNTP server. Construct with New; the
// listener is closed automatically via testing.TB.Cleanup.
type Scripted struct {
	ln net.Listener
	t  testing.TB

	mu       sync.Mutex
	articles map[string][]byte
	failures map[string]FailureMode

	wg        sync.WaitGroup
	closed    chan struct{}
	closeOnce sync.Once
}

// New starts a Scripted server bound to 127.0.0.1 on an ephemeral
// port. Call AddArticle to populate the corpus and ServerConfig to
// get a config.ServerConfig pointing at it.
func New(t testing.TB) *Scripted {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("nntptest: listen: %v", err)
	}
	s := &Scripted{
		ln:       ln,
		t:        t,
		articles: make(map[string][]byte),
		failures: make(map[string]FailureMode),
		closed:   make(chan struct{}),
	}
	t.Cleanup(s.Close)
	s.wg.Add(1)
	go s.accept()
	return s
}

// Addr returns the host:port the server is listening on.
func (s *Scripted) Addr() string { return s.ln.Addr().String() }

// ServerConfig returns a config.ServerConfig pointing at the fake.
// name and connections are caller-specified; everything else is
// filled with defaults that work against the fake.
func (s *Scripted) ServerConfig(name string, connections int) config.ServerConfig {
	// ln.Addr() returns a TCPAddr formatted as host:port; both parses
	// are trusted. Any failure here is a programmer error in net/* itself.
	host, portStr, err := net.SplitHostPort(s.Addr())
	if err != nil {
		s.t.Fatalf("nntptest: split addr %q: %v", s.Addr(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		s.t.Fatalf("nntptest: atoi %q: %v", portStr, err)
	}
	return config.ServerConfig{
		Name:               name,
		Host:               host,
		Port:               port,
		Connections:        connections,
		Enable:             true,
		Timeout:            5,
		PipeliningRequests: 1,
	}
}

// AddArticle stores body under msgID. The body is plain bytes; the
// server handles CRLF framing and dot-stuffing at the wire level.
// Lines are split on '\n' (with an optional preceding '\r' trimmed).
func (s *Scripted) AddArticle(msgID string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.articles[msgID] = body
}

// InjectFailure schedules a one-shot failure for the next Fetch of
// msgID. Passing FailureNone clears any prior entry.
func (s *Scripted) InjectFailure(msgID string, mode FailureMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode == FailureNone {
		delete(s.failures, msgID)
		return
	}
	s.failures[msgID] = mode
}

// Close shuts the listener down and waits for all connection
// goroutines to exit. Safe to call multiple times; idempotent.
func (s *Scripted) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.ln.Close() //nolint:errcheck // test teardown
	})
	s.wg.Wait()
}

func (s *Scripted) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.serve(c)
	}
}

func (s *Scripted) serve(c net.Conn) {
	defer s.wg.Done()
	defer closeConn(c)

	w := bufio.NewWriter(c)
	r := bufio.NewReader(c)

	send(w, "200 welcome\r\n")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "CAPABILITIES"):
			send(w,
				"101 capability list follows\r\n",
				"VERSION 2\r\n",
				"READER\r\n",
				".\r\n")
		case strings.HasPrefix(upper, "AUTHINFO USER"):
			send(w, "381 need password\r\n")
		case strings.HasPrefix(upper, "AUTHINFO PASS"):
			send(w, "281 authenticated\r\n")
		case strings.HasPrefix(upper, "MODE READER"):
			send(w, "200 reader mode\r\n")
		case strings.HasPrefix(upper, "BODY"), strings.HasPrefix(upper, "ARTICLE"):
			if !s.handleBody(w, c, cmd) {
				return // connection forcibly closed by failure mode
			}
		case strings.HasPrefix(upper, "QUIT"):
			send(w, "205 bye\r\n")
			return
		default:
			send(w, "500 unknown command\r\n")
		}
	}
}

// handleBody services a BODY or ARTICLE command. Returns false if it
// closed the underlying connection (forcing the caller to return).
func (s *Scripted) handleBody(w *bufio.Writer, c net.Conn, cmd string) bool {
	id := extractMsgID(cmd)

	s.mu.Lock()
	body, have := s.articles[id]
	mode := s.failures[id]
	if mode != FailureNone {
		delete(s.failures, id)
	}
	s.mu.Unlock()

	switch mode {
	case FailureNotFound:
		send(w, "430 no such article\r\n")
		return true
	case FailureDropMidBody:
		if !have {
			body = []byte("truncated")
		}
		send(w, fmt.Sprintf("222 0 <%s> body follows\r\n", id))
		prefix := body
		if len(prefix) > 3 {
			prefix = prefix[:3]
		}
		sendBytes(w, dotStuffed(prefix))
		closeConn(c)
		return false
	case FailureStall:
		send(w, fmt.Sprintf("222 0 <%s> body follows\r\n", id))
		<-s.closed
		return false
	}

	if !have {
		send(w, "430 no such article\r\n")
		return true
	}

	send(w, fmt.Sprintf("222 0 <%s> body follows\r\n", id))
	sendBytes(w, dotStuffed(body))
	send(w, ".\r\n")
	return true
}

func extractMsgID(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return ""
	}
	return strings.Trim(fields[1], "<>")
}

// send writes each part to w and flushes, ignoring errors. Write
// failures on the fake are always "client hung up" — a valid outcome
// for a server that the fake treats as "drop the connection and move
// on". Centralised so the errcheck suppression lives in one place.
func send(w *bufio.Writer, parts ...string) {
	for _, p := range parts {
		_, _ = w.WriteString(p) //nolint:errcheck // fake: write failure = client hung up
	}
	_ = w.Flush() //nolint:errcheck // fake: write failure = client hung up
}

func sendBytes(w *bufio.Writer, b []byte) {
	_, _ = w.Write(b) //nolint:errcheck // fake: write failure = client hung up
	_ = w.Flush()     //nolint:errcheck // fake: write failure = client hung up
}

func closeConn(c net.Conn) {
	_ = c.Close() //nolint:errcheck // fake: close failure is not actionable
}

// dotStuffed returns body reframed with CRLF line endings and any
// leading '.' on a line doubled per RFC 3977 section 3.1.1.
func dotStuffed(body []byte) []byte {
	lines := strings.Split(string(body), "\n")
	var out strings.Builder
	out.Grow(len(body) + len(lines)*2)
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		// Skip the empty trailing element produced by a terminating '\n',
		// otherwise we emit an extra CRLF before the terminator.
		if i == len(lines)-1 && line == "" {
			break
		}
		if strings.HasPrefix(line, ".") {
			out.WriteByte('.')
		}
		out.WriteString(line)
		out.WriteString("\r\n")
	}
	return []byte(out.String())
}
