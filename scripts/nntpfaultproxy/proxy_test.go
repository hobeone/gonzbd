package main

import (
	"bufio"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/test/mocknntp"
)

// startProxy starts the fault proxy listening on an ephemeral port with the
// given rules, pointed at upstream (a running mocknntp.Server). Returns the
// proxy's listen address.
func startProxy(t *testing.T, upstream *mocknntp.Server, rules []Rule) string {
	t.Helper()
	host, portStr, err := net.SplitHostPort(upstream.Addr())
	if err != nil {
		t.Fatalf("split upstream addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	cfg := &Config{
		Listen:   "127.0.0.1:0",
		Upstream: UpstreamConfig{Host: host, Port: port},
		Rules:    rules,
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	h := newConnHandler(cfg, slog.New(slog.DiscardHandler), 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go h.handle(conn)
		}
	}()

	return ln.Addr().String()
}

// dialRaw connects to addr, reads the greeting line, and returns the raw
// connection plus a bufio.Reader/Writer pair for issuing commands.
func dialRaw(t *testing.T, addr string) (net.Conn, *bufio.Reader, *bufio.Writer) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	return conn, br, bw
}

func sendCommand(t *testing.T, bw *bufio.Writer, line string) {
	t.Helper()
	if _, err := bw.WriteString(line); err != nil {
		t.Fatalf("write command: %v", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush command: %v", err)
	}
}

func readBody(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var body strings.Builder
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if l == ".\r\n" {
			return body.String()
		}
		body.WriteString(l)
	}
}

func TestProxy_PassthroughBody(t *testing.T) {
	upstream := mocknntp.NewServer(mocknntp.Config{})
	upstream.AddArticle("good@test", mocknntp.EncodeYEnc("file.bin", []byte("hello world")))
	if err := upstream.Start(); err != nil {
		t.Fatalf("start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })

	addr := startProxy(t, upstream, nil)
	_, br, bw := dialRaw(t, addr)

	sendCommand(t, bw, "BODY <good@test>\r\n")
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(status, "222") {
		t.Fatalf("status = %q, want 222 prefix", status)
	}
	if body := readBody(t, br); body == "" {
		t.Fatal("expected non-empty relayed body")
	}
}

func TestProxy_DropFault(t *testing.T) {
	upstream := mocknntp.NewServer(mocknntp.Config{})
	upstream.AddArticle("target@test", mocknntp.EncodeYEnc("file.bin", []byte("hello world")))
	if err := upstream.Start(); err != nil {
		t.Fatalf("start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })

	addr := startProxy(t, upstream, []Rule{
		{MessageIDs: []string{"target@test"}, Action: "drop"},
	})
	_, br, bw := dialRaw(t, addr)

	sendCommand(t, bw, "BODY <target@test>\r\n")
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(status, "430") {
		t.Fatalf("status = %q, want 430 (fault should trigger without touching upstream)", status)
	}
}

func TestProxy_CorruptFault(t *testing.T) {
	original := []byte(strings.Repeat("hello world ", 50))
	encoded := mocknntp.EncodeYEnc("file.bin", original)

	upstream := mocknntp.NewServer(mocknntp.Config{})
	upstream.AddArticle("target@test", encoded)
	if err := upstream.Start(); err != nil {
		t.Fatalf("start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })

	addr := startProxy(t, upstream, []Rule{
		{MessageIDs: []string{"target@test"}, Action: "corrupt", CorruptBytes: 20},
	})
	_, br, bw := dialRaw(t, addr)

	sendCommand(t, bw, "BODY <target@test>\r\n")
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(status, "222") {
		t.Fatalf("status = %q, want 222 (corrupt still returns a successful body)", status)
	}

	// The passthrough path (proven by TestProxy_PassthroughBody) relays bytes
	// unchanged, so the correct uncorrupted reference is exactly string(encoded):
	// EncodeYEnc already emits CRLF line endings, so no transformation is needed.
	body := readBody(t, br)
	if body == string(encoded) {
		t.Fatal("corrupted body is byte-identical to the uncorrupted upstream body")
	}
}

func TestProxy_ConcurrentCorruptNoRace(t *testing.T) {
	original := []byte(strings.Repeat("hello world ", 50))
	encoded := mocknntp.EncodeYEnc("file.bin", original)

	upstream := mocknntp.NewServer(mocknntp.Config{})
	upstream.AddArticle("target@test", encoded)
	if err := upstream.Start(); err != nil {
		t.Fatalf("start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })

	// Rate: 1.0 forces every BODY through the rng-driven matchRule path, and
	// "corrupt" forces every match through corruptLines' rng — so concurrent
	// connections hammer the per-connection rng derivation simultaneously.
	addr := startProxy(t, upstream, []Rule{
		{Rate: 1.0, Action: "corrupt", CorruptBytes: 20},
	})

	const conns = 8
	var wg sync.WaitGroup
	for range conns {
		wg.Go(func() {
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Errorf("dial proxy: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			br := bufio.NewReader(conn)
			bw := bufio.NewWriter(conn)
			if _, err := br.ReadString('\n'); err != nil { // greeting
				t.Errorf("read greeting: %v", err)
				return
			}
			if _, err := bw.WriteString("BODY <target@test>\r\n"); err != nil {
				t.Errorf("write command: %v", err)
				return
			}
			if err := bw.Flush(); err != nil {
				t.Errorf("flush command: %v", err)
				return
			}
			if _, err := br.ReadString('\n'); err != nil { // status line
				t.Errorf("read status: %v", err)
				return
			}
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					t.Errorf("read body: %v", err)
					return
				}
				if l == ".\r\n" {
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestProxy_TimeoutFault(t *testing.T) {
	upstream := mocknntp.NewServer(mocknntp.Config{})
	upstream.AddArticle("target@test", mocknntp.EncodeYEnc("file.bin", []byte("hello world")))
	if err := upstream.Start(); err != nil {
		t.Fatalf("start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })

	addr := startProxy(t, upstream, []Rule{
		{MessageIDs: []string{"target@test"}, Action: "timeout", TimeoutAfter: 50 * time.Millisecond},
	})
	conn, br, bw := dialRaw(t, addr)

	sendCommand(t, bw, "BODY <target@test>\r\n")
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("expected the connection to close without a response after the timeout fault, got a response")
	}
}

func TestTimeoutDuration(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
		want time.Duration
	}{
		{"configured value is used", Rule{TimeoutAfter: 50 * time.Millisecond}, 50 * time.Millisecond},
		{"zero defaults to 60s", Rule{}, 60 * time.Second},
		{"negative defaults to 60s", Rule{TimeoutAfter: -1}, 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := timeoutDuration(tt.rule); got != tt.want {
				t.Errorf("timeoutDuration(%+v) = %v, want %v", tt.rule, got, tt.want)
			}
		})
	}
}

// TestProxy_CorruptFaultStat exercises the corrupt fault's STAT branch: a
// matching "corrupt" rule against a STAT command must pass the (single-line,
// bodyless) response through unchanged, since corruptLines only ever
// mutates a multiline BODY response.
func TestProxy_CorruptFaultStat(t *testing.T) {
	upstream := mocknntp.NewServer(mocknntp.Config{})
	upstream.AddArticle("target@test", mocknntp.EncodeYEnc("file.bin", []byte("hello world")))
	if err := upstream.Start(); err != nil {
		t.Fatalf("start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })

	addr := startProxy(t, upstream, []Rule{
		{MessageIDs: []string{"target@test"}, Action: "corrupt", CorruptBytes: 20},
	})
	_, br, bw := dialRaw(t, addr)

	sendCommand(t, bw, "STAT <target@test>\r\n")
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(status, "223") {
		t.Fatalf("status = %q, want 223 (STAT is unaffected by the corrupt fault)", status)
	}
}
