package nntp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

// mockServer is a single-connection scripted NNTP server used by the
// tests in this file. It listens on 127.0.0.1:0 so real socket
// semantics (deadlines, CRLF framing) are exercised.
type mockServer struct {
	ln   net.Listener
	addr string
	t    *testing.T

	// exchange runs on the server side once the client connects. It
	// receives a helper that wraps Read/Write with line-oriented
	// conveniences.
	exchange func(*mockConn)
}

type mockConn struct {
	c net.Conn
	r *bufio.Reader
	t *testing.T
}

// newMockServer starts a listener and serves exactly one connection
// with the given exchange, then closes. Callers pass the returned
// host:port to Dial.
func newMockServer(t *testing.T, exchange func(*mockConn)) *mockServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ms := &mockServer{ln: ln, addr: ln.Addr().String(), t: t, exchange: exchange}
	t.Cleanup(func() { _ = ln.Close() })
	go ms.serve()
	return ms
}

func (ms *mockServer) serve() {
	c, err := ms.ln.Accept()
	if err != nil {
		return // listener closed in test cleanup
	}
	defer func() { _ = c.Close() }()
	mc := &mockConn{c: c, r: bufio.NewReader(c), t: ms.t}
	ms.exchange(mc)
}

func (m *mockConn) send(line string) {
	m.t.Helper()
	if _, err := m.c.Write([]byte(line + "\r\n")); err != nil {
		m.t.Errorf("mock write: %v", err)
	}
}

func (m *mockConn) sendRaw(b string) {
	m.t.Helper()
	if _, err := m.c.Write([]byte(b)); err != nil {
		m.t.Errorf("mock write: %v", err)
	}
}

// readLine reads one CRLF-terminated line, trims CRLF, returns it.
func (m *mockConn) readLine() string {
	m.t.Helper()
	line, err := m.r.ReadString('\n')
	if err != nil {
		m.t.Errorf("mock read: %v", err)
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// expect asserts the next line starts with prefix.
func (m *mockConn) expect(prefix string) {
	m.t.Helper()
	got := m.readLine()
	if !strings.HasPrefix(got, prefix) {
		m.t.Errorf("expected prefix %q, got %q", prefix, got)
	}
}

// sendCaps emits a canonical READER capabilities body.
func (m *mockConn) sendCaps() {
	m.send("101 capability list follows")
	m.send("VERSION 2")
	m.send("READER")
	m.send(".")
}

func makeCfg(addr string) config.ServerConfig {
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	return config.ServerConfig{
		Name:               "test",
		Host:               host,
		Port:               port,
		Connections:        1,
		Enable:             true,
		PipeliningRequests: 2,
		Timeout:            5,
	}
}

func TestDialAndFetch(t *testing.T) {
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		c.expect("BODY <abc@host>")
		c.send("222 0 <abc@host> body follows")
		c.sendRaw("hello world\r\n")
		c.sendRaw("..dotted line\r\n")
		c.sendRaw(".\r\n")
		c.expect("QUIT")
		c.send("205 bye")
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := Dial(ctx, makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	body, err := conn.Fetch(ctx, "abc@host")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := "hello world\n.dotted line\n"
	if string(body) != want {
		t.Errorf("body = %q, want %q", string(body), want)
	}

	if err := conn.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestDialAuthSuccess(t *testing.T) {
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("AUTHINFO USER alice")
		c.send("381 password required")
		c.expect("AUTHINFO PASS secret")
		c.send("281 authenticated")
		c.expect("CAPABILITIES")
		c.sendCaps()
		c.expect("QUIT")
		c.send("205 bye")
	})
	cfg := makeCfg(ms.addr)
	cfg.Username = "alice"
	cfg.Password = "secret"

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = conn.Close()
}

func TestDialAuthRejected(t *testing.T) {
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("AUTHINFO USER alice")
		c.send("381 password required")
		c.expect("AUTHINFO PASS wrong")
		c.send("481 auth rejected")
	})
	cfg := makeCfg(ms.addr)
	cfg.Username = "alice"
	cfg.Password = "wrong"

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := Dial(ctx, cfg)
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("Dial err = %v, want ErrAuthRejected", err)
	}
}

func TestDialOneShotAuth(t *testing.T) {
	// Some servers grant auth in one step with 281 after USER only.
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("AUTHINFO USER alice")
		c.send("281 authenticated")
		c.expect("CAPABILITIES")
		c.sendCaps()
	})
	cfg := makeCfg(ms.addr)
	cfg.Username = "alice"

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = conn.Close()
}

func TestFetchNoArticle(t *testing.T) {
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		c.expect("BODY <gone@host>")
		c.send("430 no such article")
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_, err = conn.Fetch(ctx, "gone@host")
	if !errors.Is(err, ErrNoArticle) {
		t.Fatalf("Fetch err = %v, want ErrNoArticle", err)
	}
}

func TestStat(t *testing.T) {
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		c.expect("STAT <ok@host>")
		c.send("223 0 <ok@host>")
		c.expect("STAT <gone@host>")
		c.send("430 no such article")
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.Stat(ctx, "ok@host"); err != nil {
		t.Errorf("Stat(ok): %v", err)
	}
	if err := conn.Stat(ctx, "gone@host"); !errors.Is(err, ErrNoArticle) {
		t.Errorf("Stat(gone) = %v, want ErrNoArticle", err)
	}
}

func TestPipelinedFetches(t *testing.T) {
	const n = 5
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		// Receive n requests and reply in order; we don't control
		// the interleaving from the client side, only the order of
		// requests-received, which the protocol guarantees matches
		// the order of responses-sent.
		for range n {
			line := c.readLine()
			id := strings.TrimSuffix(strings.TrimPrefix(line, "BODY <"), ">")
			c.send(fmt.Sprintf("222 0 <%s> body follows", id))
			c.sendRaw(fmt.Sprintf("body-for-%s\r\n.\r\n", id))
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			id := fmt.Sprintf("m%d@h", i)
			body, err := conn.Fetch(ctx, id)
			if err != nil {
				errs <- fmt.Errorf("fetch %s: %w", id, err)
				return
			}
			want := fmt.Sprintf("body-for-%s\n", id)
			if string(body) != want {
				errs <- fmt.Errorf("body for %s = %q, want %q", id, body, want)
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestFetchContextCancel(t *testing.T) {
	// Server accepts the BODY command but replies slowly; the
	// caller's ctx cancels before the response arrives. The next
	// Fetch on the connection must still work — the reader drains
	// the orphaned response and the semaphore slot is freed.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	requestReceived := make(chan struct{})
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		c.expect("BODY <slow@host>")
		close(requestReceived) // signal that the server received the BODY command
		<-release
		c.send("222 0 <slow@host> body follows")
		c.sendRaw("slow\r\n.\r\n")
		c.expect("BODY <next@host>")
		c.send("222 0 <next@host> body follows")
		c.sendRaw("next\r\n.\r\n")
	})
	conn, err := Dial(t.Context(), makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() {
		releaseFn() // unblock the server if the test bails early
		_ = conn.Close()
	})

	cancelCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := conn.Fetch(cancelCtx, "slow@host")
		done <- err
	}()
	// Wait for the mock server to confirm it received the BODY command.
	select {
	case <-requestReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mock to receive BODY request")
	}
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch cancel = %v, want Canceled", err)
	}

	// Let the server send the slow response; the reader discards it.
	releaseFn()
	// The reader goroutine will drain the orphaned response in the
	// background; the next Fetch's timeout provides the safety net.

	ctx2, cancel2 := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel2()
	body, err := conn.Fetch(ctx2, "next@host")
	if err != nil {
		t.Fatalf("Fetch after cancel: %v", err)
	}
	if string(body) != "next\n" {
		t.Errorf("body = %q, want %q", body, "next\n")
	}
}

func TestGreetingRejected(t *testing.T) {
	ms := newMockServer(t, func(c *mockConn) {
		c.send("502 service permanently unavailable")
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := Dial(ctx, makeCfg(ms.addr))
	if err == nil {
		t.Fatal("Dial should have failed on 502 greeting")
	}
	if se, ok := errors.AsType[*ServerError](err); !ok || se.Code != 502 {
		t.Errorf("err = %v, want ServerError{502}", err)
	}
}

// TestFetchStatAfterReaderError verifies that Fetch and Stat propagate the
// underlying reader error (via closeError) when the connection dies for a
// reason other than an explicit Close() call, rather than the generic
// ErrClosed.
func TestFetchStatAfterReaderError(t *testing.T) {
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		// Close the connection without responding further, causing the
		// reader to observe an unexpected EOF.
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Wait for runReader to observe the closed socket and record closeErr.
	<-conn.ctx.Done()

	wantErr := conn.closeError()
	if errors.Is(wantErr, ErrClosed) {
		t.Fatalf("closeError() = %v, want a specific reader error, not generic ErrClosed", wantErr)
	}

	if _, err := conn.Fetch(ctx, "anything@host"); !errors.Is(err, wantErr) {
		t.Errorf("Fetch after reader error = %v, want %v", err, wantErr)
	}
	if err := conn.Stat(ctx, "anything@host"); !errors.Is(err, wantErr) {
		t.Errorf("Stat after reader error = %v, want %v", err, wantErr)
	}
}

func TestFetchAfterClose(t *testing.T) {
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		c.expect("QUIT")
		c.send("205 bye")
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = conn.Close()
	_, err = conn.Fetch(ctx, "anything@host")
	if !errors.Is(err, ErrInvalidState) {
		t.Errorf("Fetch after close = %v, want ErrInvalidState", err)
	}
}

func TestStateTransitions(t *testing.T) {
	tests := []struct {
		name string
		from State
		to   State
		ok   bool
	}{
		{"disc→conn", StateDisconnected, StateConnected, true},
		{"disc→ready", StateDisconnected, StateReady, false},
		{"conn→auth", StateConnected, StateAuthenticated, true},
		{"conn→ready", StateConnected, StateReady, true}, // no-auth path
		{"auth→ready", StateAuthenticated, StateReady, true},
		{"ready→conn", StateReady, StateConnected, true}, // 480 re-auth
		{"ready→closed", StateReady, StateClosed, true},
		{"closed→anything", StateClosed, StateReady, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.from.canTransition(tc.to)
			if got != tc.ok {
				t.Errorf("%s.canTransition(%s) = %v, want %v",
					tc.from, tc.to, got, tc.ok)
			}
		})
	}
}

func TestStateString(t *testing.T) {
	tests := map[State]string{
		StateDisconnected:  "disconnected",
		StateConnected:     "connected",
		StateAuthenticated: "authenticated",
		StateReady:         "ready",
		StateClosed:        "closed",
		State(99):          "state(99)",
	}
	for s, want := range tests {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestBuildTLSConfigLevels(t *testing.T) {
	tests := []struct {
		name          string
		verify        config.SSLVerify
		wantSkip      bool
		wantVerifyCB  bool
		wantServerLen int
	}{
		{"none", config.SSLVerifyNone, true, false, len("example.com")},
		{"minimal", config.SSLVerifyMinimal, true, true, len("example.com")},
		{"hostname", config.SSLVerifyHostname, false, false, len("example.com")},
		{"strict", config.SSLVerifyStrict, false, false, len("example.com")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := buildTLSConfig("example.com", tc.verify, "")
			if err != nil {
				t.Fatalf("buildTLSConfig: %v", err)
			}
			if cfg.InsecureSkipVerify != tc.wantSkip {
				t.Errorf("InsecureSkipVerify = %v, want %v",
					cfg.InsecureSkipVerify, tc.wantSkip)
			}
			if (cfg.VerifyConnection != nil) != tc.wantVerifyCB {
				t.Errorf("VerifyConnection set = %v, want %v",
					cfg.VerifyConnection != nil, tc.wantVerifyCB)
			}
			if cfg.MinVersion < 0x0303 /* TLS 1.2 */ {
				t.Errorf("MinVersion = %#x, want >= TLS1.2", cfg.MinVersion)
			}
			if len(cfg.ServerName) != tc.wantServerLen {
				t.Errorf("ServerName len = %d, want %d",
					len(cfg.ServerName), tc.wantServerLen)
			}
		})
	}
}

func TestBuildTLSConfigCiphers(t *testing.T) {
	// Known cipher should parse; unknown should fail; custom cipher
	// forces MaxVersion to TLS 1.2 per spec §3.3.
	cfg, err := buildTLSConfig("h", config.SSLVerifyHostname,
		"ECDHE-RSA-AES128-GCM-SHA256:ECDHE-RSA-AES256-GCM-SHA384")
	if err != nil {
		t.Fatalf("parse ciphers: %v", err)
	}
	if len(cfg.CipherSuites) != 2 {
		t.Errorf("CipherSuites len = %d, want 2", len(cfg.CipherSuites))
	}
	if cfg.MaxVersion != 0x0303 /* TLS 1.2 */ {
		t.Errorf("MaxVersion with custom ciphers = %#x, want TLS1.2", cfg.MaxVersion)
	}

	if _, err := buildTLSConfig("h", config.SSLVerifyHostname, "NO-SUCH-CIPHER"); err == nil {
		t.Error("unknown cipher should error")
	}
}

func TestParseCapabilities(t *testing.T) {
	caps := parseCapabilities("VERSION 2\r\nREADER\r\nPOST\r\nSTAT\r\n")
	if !caps.HasBody || !caps.HasStat {
		t.Errorf("READER caps should yield HasBody+HasStat, got %+v", caps)
	}
	if !caps.HasPost {
		t.Error("HasPost should be true")
	}
	if caps.Version != 2 {
		t.Errorf("Version = %d, want 2", caps.Version)
	}
	if len(caps.Raw) != 4 {
		t.Errorf("Raw len = %d, want 4", len(caps.Raw))
	}
}

func TestReadDotStuffedBody(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"plain", "line1\r\nline2\r\n.\r\n", "line1\nline2\n"},
		{"dot-stuffed", "..hidden\r\n.\r\n", ".hidden\n"},
		{"empty", ".\r\n", ""},
		{"mixed-lf", "a\nb\r\n.\r\n", "a\nb\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tc.wire))
			got, err := readDotStuffedBody(r)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReadDotStuffedBody_ExceedsMaxSize verifies that readDotStuffedBody
// returns an error when the total body exceeds maxBodySize (10 MB).
func TestReadDotStuffedBody_ExceedsMaxSize(t *testing.T) {
	// Build a reader that produces lines of ~1000 bytes each, exceeding 10 MB
	// total without a dot-terminator before the limit.
	lineLen := 1000
	line := strings.Repeat("A", lineLen) + "\r\n"
	// We need enough lines to exceed maxBodySize (10*1024*1024 bytes).
	numLines := (maxBodySize / lineLen) + 10
	var sb strings.Builder
	for range numLines {
		sb.WriteString(line)
	}
	// Add the dot-terminator after the limit — it should never be reached.
	sb.WriteString(".\r\n")

	r := bufio.NewReader(strings.NewReader(sb.String()))
	_, err := readDotStuffedBody(r)
	if err == nil {
		t.Fatal("expected error for body exceeding maxBodySize, got nil")
	}
	wantSubstr := fmt.Sprintf("body exceeds %d bytes", maxBodySize)
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error = %q, want substring %q", err.Error(), wantSubstr)
	}
}

type blockLimiter struct {
	enabled atomic.Bool
	blocked chan struct{}
	once    sync.Once
}

func (l *blockLimiter) Wait(ctx context.Context, n int) error {
	if !l.enabled.Load() {
		return nil
	}
	l.once.Do(func() { close(l.blocked) })
	<-ctx.Done()
	return ctx.Err()
}

func TestCloseUnblocksRateLimiter(t *testing.T) {
	mockDone := make(chan struct{})
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		c.expect("BODY <test@host>")
		c.send("222 0 <test@host> body follows")
		// Send a byte to trigger a Read
		c.sendRaw("abc\r\n")
		// Block until the test signals completion instead of sleeping.
		<-mockDone
	})

	lim := &blockLimiter{blocked: make(chan struct{})}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := Dial(ctx, makeCfg(ms.addr), WithLimiter(lim))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() {
		close(mockDone) // unblock mock server goroutine
		_ = conn.Close()
	})

	// Enable the limiter now that handshake is done.
	lim.enabled.Store(true)

	go func() {
		// This will trigger a Read which will block in the limiter's Wait
		_, _ = conn.Fetch(ctx, "test@host")
	}()

	// Wait until the reader enters the limiter
	select {
	case <-lim.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for limiter to block")
	}

	// Call Close; it should cancel the context and unblock Wait
	errc := make(chan error, 1)
	go func() {
		errc <- conn.Close()
	}()

	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Close deadlocked, limiter did not unblock")
	}
}

func TestFetch_WriteFailure(t *testing.T) {
	t.Parallel()
	ms := newMockServer(t, func(mc *mockConn) {
		mc.send("200 welcome")
		mc.expect("CAPABILITIES")
		mc.sendCaps()
	})

	c, err := Dial(context.Background(), makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// Close underlying network connection directly to force write/flush failure.
	c.nc.Close()

	_, err = c.Fetch(context.Background(), "<test@example.com>")
	if err == nil {
		t.Error("expected error due to closed socket, got nil")
	}

	// Verify unappendPending worked.
	c.pendingLock.Lock()
	pLen := len(c.pending)
	c.pendingLock.Unlock()
	if pLen != 0 {
		t.Errorf("expected pending queue to be empty, got %d", pLen)
	}
}

func TestFetch_AfterReaderError(t *testing.T) {
	t.Parallel()
	ms := newMockServer(t, func(mc *mockConn) {
		mc.send("200 welcome")
		mc.expect("CAPABILITIES")
		mc.sendCaps()
		// Abruptly close server connection.
		mc.c.Close()
	})

	c, err := Dial(context.Background(), makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// Wait for the reader loop to observe the socket close and cancel the
	// connection context.
	<-c.ctx.Done()

	// Now try to fetch. It should return the reader error (or wrap it).
	_, err = c.Fetch(context.Background(), "<test@example.com>")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected EOF or ErrUnexpectedEOF, got %v", err)
	}
}

func TestStat_Errors(t *testing.T) {
	// 1. State != StateReady
	{
		conn := &Conn{} // state is StateInit (0), not StateReady
		err := conn.Stat(t.Context(), "test@host")
		if !errors.Is(err, ErrInvalidState) {
			t.Errorf("expected ErrInvalidState, got %v", err)
		}
	}

	// 2. Already cancelled context
	{
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		conn := &Conn{state: StateReady}
		err := conn.Stat(ctx, "test@host")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	}
}

func TestStat_ServerErrors(t *testing.T) {
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()

		// Case A: unexpected 500 error
		c.expect("STAT <error@host>")
		c.send("500 internal server error")

		// Case B: timeout context / orphan
		c.expect("STAT <timeout@host>")
		// Allow client context to time out (50ms), then send response to keep pipeline in sync.
		time.Sleep(200 * time.Millisecond)
		c.send("223 0 <timeout@host>")

		// Case C: unexpected code 300 (non-sentinel)
		c.expect("STAT <code300@host>")
		c.send("300 special response")
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Test case A
	err = conn.Stat(ctx, "error@host")
	if !errors.Is(err, ErrTransient) {
		t.Errorf("expected ErrTransient error, got %v", err)
	}

	// Test case B (context timeout mid-flight)
	queryCtx, queryCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer queryCancel()
	err = conn.Stat(queryCtx, "timeout@host")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}

	// Test case C (unexpected code 300)
	err = conn.Stat(ctx, "code300@host")
	if se, ok := errors.AsType[*ServerError](err); !ok || se.Code != 300 {
		t.Errorf("expected ServerError 300, got %v", err)
	}
}

// TestFetchMessageIDMismatch pins B1/F5: the reader must compare the
// Message-ID echoed on a 222 line against the one the corresponding
// request asked for, and treat a disagreement as a desynced connection
// rather than as one bad article.
//
// The server here reads BOTH pipelined BODY commands before answering
// either — which makes the FIFO order known to the test — and then
// answers the FIRST one with the SECOND one's Message-ID. Matching by
// FIFO position alone accepts that response and hands the caller a
// perfectly well-formed body belonging to a different article; nothing
// downstream can detect it, because the bytes are internally consistent.
//
// Both callers must therefore fail: the first on the mismatch itself,
// the second because the connection it was queued on is now known to be
// desynced. The server deliberately sends only the one response, so the
// test never depends on a write to an already-closed socket.
func TestFetchMessageIDMismatch(t *testing.T) {
	const n = 2
	serverDone := make(chan struct{})
	t.Cleanup(func() { close(serverDone) })

	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		// Read both requests first: the protocol guarantees the FIFO
		// order matches the order they arrive in, so ids[0] is the
		// request the first response will be paired with.
		ids := make([]string, n)
		for i := range ids {
			line := c.readLine()
			ids[i] = strings.TrimSuffix(strings.TrimPrefix(line, "BODY <"), ">")
		}
		// Answer request ids[0] with request ids[1]'s identity.
		c.send(fmt.Sprintf("222 0 <%s> body follows", ids[1]))
		c.sendRaw(fmt.Sprintf("body-for-%s\r\n.\r\n", ids[1]))
		<-serverDone
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			_, ferr := conn.Fetch(ctx, fmt.Sprintf("m%d@h", i))
			errs <- ferr
		})
	}
	wg.Wait()
	close(errs)

	var mismatch int
	for e := range errs {
		if e == nil {
			t.Fatal("Fetch succeeded on a connection whose 222 line named a different article")
		}
		if strings.Contains(e.Error(), "Message-ID mismatch") {
			mismatch++
		}
	}
	if mismatch == 0 {
		t.Errorf("no Fetch reported a Message-ID mismatch; errors were not attributable to the desync")
	}
	// The connection must be unusable afterwards, not merely have
	// failed the two commands in flight. Note that this asserts on a
	// subsequent Fetch rather than on State(): finishReader records
	// the failure in c.closed and does not advance the lifecycle
	// state, which is true of every reader-side failure and not
	// specific to a desync.
	if _, err := conn.Fetch(ctx, "after@h"); err == nil {
		t.Error("Fetch succeeded after a desync; the connection was not dropped")
	}
}

// TestStatMessageIDMismatchDropsConnection pins the second half of
// B1/F5 — that a mismatch kills the connection rather than failing one
// command — which TestFetchMessageIDMismatch cannot isolate.
//
// With a 222 the reader must consume a body, so ANY early exit from
// the mismatch branch leaves that body to be read as the next status
// line; the connection then dies of a parse error whether or not the
// mismatch itself dropped it. STAT's 223 carries no body, so the
// stream stays perfectly parseable across a mismatch: without the
// drop, the reader would sail on and the SECOND Stat would succeed —
// answered, unnoticed, by a response belonging to the first.
func TestStatMessageIDMismatchDropsConnection(t *testing.T) {
	const n = 2
	serverDone := make(chan struct{})
	t.Cleanup(func() { close(serverDone) })

	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		ids := make([]string, n)
		for i := range ids {
			line := c.readLine()
			ids[i] = strings.TrimSuffix(strings.TrimPrefix(line, "STAT <"), ">")
		}
		// Both responses are well-formed and body-less; only the
		// pairing is wrong, and it is wrong by exactly one place.
		c.send(fmt.Sprintf("223 0 <%s>", ids[1]))
		c.send(fmt.Sprintf("223 0 <%s>", ids[1]))
		<-serverDone
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, makeCfg(ms.addr))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() { errs <- conn.Stat(ctx, fmt.Sprintf("s%d@h", i)) })
	}
	wg.Wait()
	close(errs)

	for e := range errs {
		if e == nil {
			t.Fatal("Stat reported an article present on the strength of a 223 naming a different one")
		}
	}
}
