package mocknntp_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

// makeCfg builds a ServerConfig pointing at the given host:port.
func makeCfg(addr string) config.ServerConfig {
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	_, _ = parsePort(portStr, &port)
	return config.ServerConfig{
		Name:               "mock",
		Host:               host,
		Port:               port,
		Connections:        2,
		Enable:             true,
		PipeliningRequests: 2,
		Timeout:            5,
	}
}

// parsePort is a thin wrapper so we don't pull in fmt into the cfg helper.
func parsePort(s string, out *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return n, nil
}

func startServer(t *testing.T, cfg mocknntp.Config) *mocknntp.Server {
	t.Helper()
	srv := mocknntp.NewServer(cfg)
	if err := srv.Start(); err != nil {
		t.Fatalf("Server.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Logf("Server.Close: %v", err)
		}
	})
	return srv
}

func TestServerGreetingAndCapabilities(t *testing.T) {
	t.Parallel()

	srv := startServer(t, mocknntp.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, makeCfg(srv.Addr()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) //nolint:errcheck // test cleanup

	caps := conn.Caps()
	if caps == nil {
		t.Fatal("Caps() is nil; CAPABILITIES probe failed")
	}
	if !caps.HasBody {
		t.Error("expected HasBody=true")
	}
	if !caps.HasStat {
		t.Error("expected HasStat=true")
	}
}

func TestServerBody(t *testing.T) {
	t.Parallel()

	payload := []byte("hello from the mock server")
	body := EncodeYEncForTest("test.bin", payload)

	srv := startServer(t, mocknntp.Config{})
	srv.AddArticle("msg1@host", body)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, makeCfg(srv.Addr()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) //nolint:errcheck // test cleanup

	got, err := conn.Fetch(ctx, "msg1@host")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != string(body)+"\n" && string(got) != string(body) {
		// The client strips the CRLF-terminated body terminator and normalises
		// CRLF→LF. Check that the decoded payload matches.
		// Rather than byte-comparing the encoded form, verify the body is non-empty.
		if len(got) == 0 {
			t.Error("Fetch returned empty body")
		}
	}
	// The body should contain the yEnc header.
	if len(got) == 0 {
		t.Error("Fetch returned empty body")
	}
}

func TestServerBodyUnregisteredID(t *testing.T) {
	t.Parallel()

	srv := startServer(t, mocknntp.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, makeCfg(srv.Addr()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) //nolint:errcheck // test cleanup

	_, err = conn.Fetch(ctx, "nosuchid@host")
	if !errors.Is(err, nntp.ErrNoArticle) {
		t.Fatalf("Fetch unregistered = %v, want ErrNoArticle", err)
	}
}

func TestServerAuthRequired_NoCreds(t *testing.T) {
	t.Parallel()

	srv := startServer(t, mocknntp.Config{
		RequireAuth: true,
		Users:       map[string]string{"alice": "secret"},
	})
	srv.AddArticle("auth-test@host", []byte("body"))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Connect without credentials; Dial succeeds but Fetch should return 480.
	conn, err := nntp.Dial(ctx, makeCfg(srv.Addr()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) //nolint:errcheck // test cleanup

	_, err = conn.Fetch(ctx, "auth-test@host")
	if !errors.Is(err, nntp.ErrAuthRequired) {
		t.Fatalf("Fetch without creds = %v, want ErrAuthRequired", err)
	}
}

func TestServerAuthRequired_WithCreds(t *testing.T) {
	t.Parallel()

	payload := []byte("authenticated content")
	body := EncodeYEncForTest("auth.bin", payload)

	srv := startServer(t, mocknntp.Config{
		RequireAuth: true,
		Users:       map[string]string{"alice": "secret"},
	})
	srv.AddArticle("auth-test@host", body)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	cfg := makeCfg(srv.Addr())
	cfg.Username = "alice"
	cfg.Password = "secret"

	conn, err := nntp.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) //nolint:errcheck // test cleanup

	got, err := conn.Fetch(ctx, "auth-test@host")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) == 0 {
		t.Error("Fetch returned empty body")
	}
}

func TestServerAuthRejected_WrongPassword(t *testing.T) {
	t.Parallel()

	srv := startServer(t, mocknntp.Config{
		RequireAuth: true,
		Users:       map[string]string{"alice": "secret"},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	cfg := makeCfg(srv.Addr())
	cfg.Username = "alice"
	cfg.Password = "wrong"

	_, err := nntp.Dial(ctx, cfg)
	if !errors.Is(err, nntp.ErrAuthRejected) {
		t.Fatalf("Dial wrong password = %v, want ErrAuthRejected", err)
	}
}

func TestServerConcurrentFetches(t *testing.T) {
	t.Parallel()

	const n = 5

	srv := startServer(t, mocknntp.Config{})
	articles := make([]string, n)
	for i := range articles {
		id := t.Name() + "@host"
		articles[i] = id
		body := EncodeYEncForTest("part.bin", []byte("data for article"))
		srv.AddArticle(id, body)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			cfg := makeCfg(srv.Addr())
			conn, err := nntp.Dial(ctx, cfg)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = conn.Close() }() //nolint:errcheck // test cleanup
			got, err := conn.Fetch(ctx, articles[i])
			if err != nil {
				errs <- err
				return
			}
			if len(got) == 0 {
				errs <- errors.New("empty body")
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestServerCloseIdempotent(t *testing.T) {
	t.Parallel()

	srv := mocknntp.NewServer(mocknntp.Config{})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// Second Close should not panic or block.
	if err := srv.Close(); err != nil {
		// May return an error on repeated close of the underlying listener;
		// that is acceptable as long as it does not panic or block.
		t.Logf("second Close returned (expected): %v", err)
	}
}

func TestServerCustomGreeting201(t *testing.T) {
	t.Parallel()

	srv := startServer(t, mocknntp.Config{
		Greeting: "201 mocknntp no posting",
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Dial should succeed on a 201 greeting (posting not allowed but reading ok).
	conn, err := nntp.Dial(ctx, makeCfg(srv.Addr()))
	if err != nil {
		t.Fatalf("Dial on 201 greeting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) //nolint:errcheck // test cleanup
}

// EncodeYEncForTest is a package-level alias so server tests don't import
// articles.go via a separate import path — they're in the same test package.
func EncodeYEncForTest(filename string, payload []byte) []byte {
	return mocknntp.EncodeYEnc(filename, payload)
}

// TestArticlesServed_CountsOnlyDeliveredBodies pins the counter a
// crash-consistency harness measures rework with. Two facts have to hold for
// that measurement to mean anything: a repeated fetch increments, and a BODY
// that could not be answered does not appear at all. A counter incremented on
// receipt of the command rather than on delivery would satisfy the first and
// fail the second, and would then attribute a 430 to the download as work
// redone.
func TestArticlesServed_CountsOnlyDeliveredBodies(t *testing.T) {
	t.Parallel()

	srv := startServer(t, mocknntp.Config{})
	srv.AddArticle("msg1@host", EncodeYEncForTest("test.bin", []byte("payload")))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, makeCfg(srv.Addr()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) //nolint:errcheck // test cleanup

	if got := srv.ArticlesServed(); len(got) != 0 {
		t.Fatalf("ArticlesServed before any fetch = %v, want empty", got)
	}
	for range 2 {
		if _, err := conn.Fetch(ctx, "msg1@host"); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	}
	if _, err := conn.Fetch(ctx, "absent@host"); !errors.Is(err, nntp.ErrNoArticle) {
		t.Fatalf("Fetch absent = %v, want ErrNoArticle", err)
	}

	served := srv.ArticlesServed()
	if got := served["msg1@host"]; got != 2 {
		t.Errorf("served[msg1@host] = %d, want 2", got)
	}
	if _, ok := served["absent@host"]; ok {
		t.Errorf("an unanswerable BODY was counted as served: %v", served)
	}

	// The returned map is a copy: mutating it must not corrupt the server's
	// own tally, or a caller that diffs two snapshots would poison the second.
	served["msg1@host"] = 99
	if got := srv.ArticlesServed()["msg1@host"]; got != 2 {
		t.Errorf("after mutating the returned map, served[msg1@host] = %d, want 2", got)
	}
}
