package nntp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/config"
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
		for i := 0; i < n; i++ {
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
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
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
		}()
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
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		c.expect("BODY <slow@host>")
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
	// Give the client a moment to submit the request.
	time.Sleep(50 * time.Millisecond)
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch cancel = %v, want Canceled", err)
	}

	// Let the server send the slow response; the reader discards it.
	releaseFn()
	// Wait briefly for the reader to consume it.
	time.Sleep(50 * time.Millisecond)

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
	var se *ServerError
	if !errors.As(err, &se) || se.Code != 502 {
		t.Errorf("err = %v, want ServerError{502}", err)
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
	ms := newMockServer(t, func(c *mockConn) {
		c.send("200 welcome")
		c.expect("CAPABILITIES")
		c.sendCaps()
		c.expect("BODY <test@host>")
		c.send("222 0 <test@host> body follows")
		// Send a byte to trigger a Read
		c.sendRaw("abc\r\n")
		// Wait indefinitely so the socket stays open, preventing socket-close from unblocking the read
		time.Sleep(2 * time.Second)
	})

	lim := &blockLimiter{blocked: make(chan struct{})}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := Dial(ctx, makeCfg(ms.addr), WithLimiter(lim))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

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
