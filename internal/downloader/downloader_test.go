package downloader

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/nntp"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// mockNNTP is a test-only NNTP server that accepts any number of
// concurrent connections and serves scripted BODY responses from an
// in-memory map. Unrecognised message IDs produce 430.
//
// It's not a faithful RFC 3977 implementation — it only speaks the
// verbs the downloader uses (CAPABILITIES, BODY, STAT, QUIT) — but
// it exercises the same TCP/CRLF plumbing that a real server would.
type mockNNTP struct {
	addr string
	ln   net.Listener

	// bodies[msgID] = raw body (without the dot-terminator).
	bodies map[string]string

	// reject[msgID] = true causes 430 responses.
	reject map[string]bool

	// bodiesMu guards the two maps above.
	bodiesMu sync.Mutex

	// requireAuth demands AUTHINFO. User/pass accepted are user/pass.
	requireAuth bool
	user, pass  string

	// bodyDelay is slept before writing any BODY response (success or
	// 430). Used to widen the in-flight window so tests can assert the
	// dispatcher does not speculatively fan out to other servers.
	bodyDelay time.Duration

	// stats
	dials      atomic.Int64
	fetches    atomic.Int64
	rejections atomic.Int64

	// wg tracks connection goroutines so tests can Shutdown cleanly.
	wg sync.WaitGroup
}

// mockOption configures a mockNNTP before the accept loop starts.
// Using options avoids races between test goroutines setting fields
// (requireAuth, user, pass) and handler goroutines reading them.
type mockOption func(*mockNNTP)

func withAuth(user, pass string) mockOption {
	return func(m *mockNNTP) {
		m.requireAuth = true
		m.user = user
		m.pass = pass
	}
}

func withBodyDelay(d time.Duration) mockOption {
	return func(m *mockNNTP) { m.bodyDelay = d }
}

func newMockNNTP(t *testing.T, opts ...mockOption) *mockNNTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ms := &mockNNTP{
		addr:   ln.Addr().String(),
		ln:     ln,
		bodies: make(map[string]string),
		reject: make(map[string]bool),
	}
	for _, o := range opts {
		o(ms)
	}
	t.Cleanup(func() { _ = ln.Close(); ms.wg.Wait() })
	go ms.acceptLoop()
	return ms
}

func (ms *mockNNTP) addArticle(msgID, body string) {
	ms.bodiesMu.Lock()
	defer ms.bodiesMu.Unlock()
	ms.bodies[msgID] = body
}

func (ms *mockNNTP) rejectArticle(msgID string) {
	ms.bodiesMu.Lock()
	defer ms.bodiesMu.Unlock()
	ms.reject[msgID] = true
}

func (ms *mockNNTP) acceptLoop() {
	for {
		c, err := ms.ln.Accept()
		if err != nil {
			return // listener closed
		}
		ms.wg.Add(1)
		go func(c net.Conn) {
			defer ms.wg.Done()
			defer func() { _ = c.Close() }()
			ms.dials.Add(1)
			ms.handleConn(c)
		}(c)
	}
}

func (ms *mockNNTP) handleConn(c net.Conn) {
	r := bufio.NewReader(c)
	write := func(s string) bool {
		_, err := c.Write([]byte(s))
		return err == nil
	}
	if !write("200 welcome\r\n") {
		return
	}
	authenticated := !ms.requireAuth
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(cmd, "AUTHINFO USER "):
			user := strings.TrimPrefix(cmd, "AUTHINFO USER ")
			if user != ms.user {
				_ = write("481 bad user\r\n")
				continue
			}
			_ = write("381 password required\r\n")
		case strings.HasPrefix(cmd, "AUTHINFO PASS "):
			pass := strings.TrimPrefix(cmd, "AUTHINFO PASS ")
			if pass != ms.pass {
				_ = write("481 bad pass\r\n")
				continue
			}
			authenticated = true
			_ = write("281 authenticated\r\n")
		case cmd == "CAPABILITIES":
			_ = write("101 capabilities\r\nVERSION 2\r\nREADER\r\n.\r\n")
		case strings.HasPrefix(cmd, "BODY "):
			if !authenticated {
				_ = write("480 authentication required\r\n")
				continue
			}
			id := strings.Trim(strings.TrimPrefix(cmd, "BODY "), "<>")
			ms.bodiesMu.Lock()
			body, hasBody := ms.bodies[id]
			rejected := ms.reject[id]
			ms.bodiesMu.Unlock()
			if ms.bodyDelay > 0 {
				time.Sleep(ms.bodyDelay)
			}
			if rejected || !hasBody {
				ms.rejections.Add(1)
				_ = write("430 no such article\r\n")
				continue
			}
			ms.fetches.Add(1)
			_ = write(fmt.Sprintf("222 0 <%s> body follows\r\n%s\r\n.\r\n", id, body))
		case strings.HasPrefix(cmd, "STAT "):
			id := strings.Trim(strings.TrimPrefix(cmd, "STAT "), "<>")
			ms.bodiesMu.Lock()
			_, hasBody := ms.bodies[id]
			ms.bodiesMu.Unlock()
			if !hasBody {
				_ = write("430 no such article\r\n")
				continue
			}
			_ = write(fmt.Sprintf("223 0 <%s>\r\n", id))
		case cmd == "QUIT":
			_ = write("205 bye\r\n")
			return
		default:
			_ = write("500 unknown command\r\n")
		}
	}
}

// testServer builds a *Server wrapping a ServerConfig that points at
// the mock listener. Name is required to distinguish multiple servers
// in try-list tests.
func testServer(t *testing.T, name, addr string, opts ...func(*config.ServerConfig)) *Server {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	cfg := config.ServerConfig{
		Name:               name,
		Host:               host,
		Port:               port,
		Connections:        2,
		PipeliningRequests: 1,
		Timeout:            5,
		Enable:             true,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return NewServer(cfg)
}

// makeJobWithArticles builds a one-file job whose articles carry the
// given Message-IDs and a trivial byte size.
func makeJobWithArticles(t *testing.T, msgIDs []string) *queue.Job {
	t.Helper()
	parsed := &nzb.NZB{
		Meta:   map[string][]string{"title": {"test"}},
		Groups: []string{"alt.binaries.test"},
		AvgAge: time.Unix(1700000000, 0),
	}
	file := nzb.File{
		Subject: "test.bin",
		Date:    time.Unix(1700000000, 0),
	}
	for i, id := range msgIDs {
		file.Articles = append(file.Articles, nzb.Article{
			ID:     id,
			Bytes:  100,
			Number: i + 1,
		})
		file.Bytes += 100
	}
	parsed.Files = []nzb.File{file}
	job, err := queue.NewJob(parsed, queue.AddOptions{
		Filename: "test.nzb",
		Priority: constants.NormalPriority,
	})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	return job
}

// collect reads up to n results from ch or errors after timeout.
func collect(t *testing.T, ch <-chan *ArticleResult, n int, timeout time.Duration) []*ArticleResult {
	t.Helper()
	results := make([]*ArticleResult, 0, n)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for i := 0; i < n; i++ {
		select {
		case r := <-ch:
			results = append(results, r)
		case <-deadline.C:
			t.Fatalf("timeout after %d results (wanted %d)", len(results), n)
		}
	}
	return results
}

func TestDownloaderHappyPath(t *testing.T) {
	ms := newMockNNTP(t)
	ms.addArticle("a@h", "body-a")
	ms.addArticle("b@h", "body-b")
	ms.addArticle("c@h", "body-c")

	q := queue.New()
	job := makeJobWithArticles(t, []string{"a@h", "b@h", "c@h"})
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	srv := testServer(t, "primary", ms.addr)
	d := New(q, []*Server{srv}, Options{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	results := collect(t, d.Completions(), 3, 5*time.Second)
	got := make(map[string]string)
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("unexpected err for %s: %v", r.MessageID, r.Err)
			continue
		}
		got[r.MessageID] = string(r.Body)
	}
	want := map[string]string{
		"a@h": "body-a\n",
		"b@h": "body-b\n",
		"c@h": "body-c\n",
	}
	for id, wantBody := range want {
		if got[id] != wantBody {
			t.Errorf("%s: got %q, want %q", id, got[id], wantBody)
		}
	}

	// Queue state reflects the successful completions.
	after, _ := q.Get(job.ID)
	for _, art := range after.Files[0].Articles {
		if !art.Done {
			t.Errorf("article %s not marked Done", art.ID)
		}
	}
}

func TestDownloaderFallbackServer(t *testing.T) {
	// Primary rejects 'a@h'; backup has it. Downloader should flip
	// to backup via the try-list.
	primary := newMockNNTP(t)
	primary.addArticle("b@h", "body-b") // primary has b only
	primary.rejectArticle("a@h")        // simulate article missing

	backup := newMockNNTP(t)
	backup.addArticle("a@h", "body-a-backup")
	backup.addArticle("b@h", "body-b-backup")

	q := queue.New()
	_ = q.Add(makeJobWithArticles(t, []string{"a@h", "b@h"}))

	srvPrimary := testServer(t, "primary", primary.addr)
	srvBackup := testServer(t, "backup", backup.addr)
	d := New(q, []*Server{srvPrimary, srvBackup}, Options{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Expect 3 results total: 430 from primary for 'a@h', then 222
	// from backup for 'a@h', then 222 from primary for 'b@h'. Order
	// is not guaranteed across servers; collect all 3.
	results := collect(t, d.Completions(), 3, 5*time.Second)

	var aSuccess, bSuccess bool
	var aRejected bool
	for _, r := range results {
		switch {
		case r.MessageID == "a@h" && r.Err == nil:
			aSuccess = true
			if string(r.Body) != "body-a-backup\n" {
				t.Errorf("a@h body = %q, want backup body", r.Body)
			}
			if r.ServerName != "backup" {
				t.Errorf("a@h served by %q, want backup", r.ServerName)
			}
		case r.MessageID == "a@h" && errors.Is(r.Err, nntp.ErrNoArticle):
			aRejected = true
			if r.ServerName != "primary" {
				t.Errorf("a@h 430 from %q, want primary", r.ServerName)
			}
		case r.MessageID == "b@h" && r.Err == nil:
			bSuccess = true
		}
	}
	if !aRejected {
		t.Error("expected 430 on primary for a@h")
	}
	if !aSuccess {
		t.Error("expected backup success for a@h")
	}
	if !bSuccess {
		t.Error("expected primary success for b@h")
	}
}

// TestDownloaderNoSpeculativeFallback verifies that while an article
// is in-flight on one server, the dispatcher does NOT concurrently
// send the same article to another server. Fallback happens only
// after the first server's response resolves.
//
// The scenario: primary rejects a@h but takes 200ms to respond.
// Backup has a@h and responds instantly. If the dispatcher is
// speculative, backup will be dialed and fetch a@h while primary is
// still sleeping. If it is sequential (correct), backup sees no
// requests until primary has rejected.
func TestDownloaderNoSpeculativeFallback(t *testing.T) {
	primary := newMockNNTP(t, withBodyDelay(200*time.Millisecond))
	primary.rejectArticle("a@h")

	backup := newMockNNTP(t)
	backup.addArticle("a@h", "body-a-backup")

	q := queue.New()
	_ = q.Add(makeJobWithArticles(t, []string{"a@h"}))

	srvPrimary := testServer(t, "primary", primary.addr)
	srvBackup := testServer(t, "backup", backup.addr)
	d := New(q, []*Server{srvPrimary, srvBackup}, Options{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// 100 ms into primary's 200 ms delay: backup must be untouched.
	time.Sleep(100 * time.Millisecond)
	if got := backup.dials.Load(); got != 0 {
		t.Fatalf("backup was dialed while primary in-flight: %d dials", got)
	}
	if got := backup.fetches.Load(); got != 0 {
		t.Fatalf("backup fetched while primary in-flight: %d fetches", got)
	}

	// Let primary finish and backup take over.
	results := collect(t, d.Completions(), 2, 5*time.Second)

	var saw430, sawSuccess bool
	for _, r := range results {
		switch {
		case r.ServerName == "primary" && errors.Is(r.Err, nntp.ErrNoArticle):
			saw430 = true
		case r.ServerName == "backup" && r.Err == nil:
			sawSuccess = true
		}
	}
	if !saw430 {
		t.Error("expected 430 from primary")
	}
	if !sawSuccess {
		t.Error("expected success from backup")
	}
	if got := primary.rejections.Load(); got != 1 {
		t.Errorf("primary rejections = %d, want 1", got)
	}
	if got := backup.fetches.Load(); got != 1 {
		t.Errorf("backup fetches = %d, want 1", got)
	}
}

func TestDownloaderPauseResume(t *testing.T) {
	ms := newMockNNTP(t)
	ms.addArticle("p@h", "body-p")

	q := queue.New()
	d := New(q, []*Server{testServer(t, "s", ms.addr)}, Options{})
	d.Pause()
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Add work while paused; nothing should be fetched.
	_ = q.Add(makeJobWithArticles(t, []string{"p@h"}))

	select {
	case r := <-d.Completions():
		t.Fatalf("got result while paused: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}

	d.Resume()
	results := collect(t, d.Completions(), 1, 5*time.Second)
	if results[0].Err != nil {
		t.Errorf("post-resume err: %v", results[0].Err)
	}
}

func TestDownloaderDialFailure(t *testing.T) {
	// Point the server config at a listener we immediately close,
	// so every Dial attempt fails. The server should accumulate bad
	// connections and (because Optional=true) get deactivated after
	// the ratio threshold is crossed.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()

	q := queue.New()
	_ = q.Add(makeJobWithArticles(t, []string{"x@h", "y@h"}))

	srv := testServer(t, "dead", addr, func(c *config.ServerConfig) {
		c.Connections = 2
		c.Optional = true
		c.Required = false
	})
	d := New(q, []*Server{srv}, Options{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Collect at least 2 results; each should be an error.
	results := collect(t, d.Completions(), 2, 5*time.Second)
	for _, r := range results {
		if r.Err == nil {
			t.Errorf("expected dial failure, got success for %s", r.MessageID)
		}
	}
	// Server should have accumulated bad connection count.
	if srv.BadConnections() < 2 {
		t.Errorf("BadConnections = %d, want >= 2", srv.BadConnections())
	}
}

func TestDownloaderAuth(t *testing.T) {
	ms := newMockNNTP(t, withAuth("alice", "secret"))
	ms.addArticle("a@h", "body-a")

	q := queue.New()
	_ = q.Add(makeJobWithArticles(t, []string{"a@h"}))

	srv := testServer(t, "auth", ms.addr, func(c *config.ServerConfig) {
		c.Username = "alice"
		c.Password = "secret"
		c.Connections = 1
	})
	d := New(q, []*Server{srv}, Options{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	results := collect(t, d.Completions(), 1, 5*time.Second)
	if results[0].Err != nil {
		t.Fatalf("auth fetch err: %v", results[0].Err)
	}
	if string(results[0].Body) != "body-a\n" {
		t.Errorf("body = %q, want %q", results[0].Body, "body-a\n")
	}
}

func TestDownloaderGracefulShutdown(t *testing.T) {
	ms := newMockNNTP(t)
	for i := 0; i < 10; i++ {
		ms.addArticle(fmt.Sprintf("a%d@h", i), fmt.Sprintf("body-%d", i))
	}
	q := queue.New()
	ids := make([]string, 10)
	for i := range ids {
		ids[i] = fmt.Sprintf("a%d@h", i)
	}
	_ = q.Add(makeJobWithArticles(t, ids))

	d := New(q, []*Server{testServer(t, "s", ms.addr)}, Options{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain some results then Stop mid-stream.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range d.Completions() {
			// Consume until Stop closes the channel.
		}
	}()

	time.Sleep(100 * time.Millisecond)
	if err := d.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Error("completions channel not drained after Stop")
	}

	// Second Stop is a no-op.
	if err := d.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

func TestDownloaderSetSpeedLimit(t *testing.T) {
	ms := newMockNNTP(t)
	ms.addArticle("a@h", "body-a")

	q := queue.New()
	_ = q.Add(makeJobWithArticles(t, []string{"a@h"}))

	d := New(q, []*Server{testServer(t, "s", ms.addr)}, Options{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Valid limit, unlimited, then valid again — all should be accepted.
	d.SetSpeedLimit(1024 * 1024)
	d.SetSpeedLimit(0)
	d.SetSpeedLimit(512 * 1024)

	results := collect(t, d.Completions(), 1, 5*time.Second)
	if results[0].Err != nil {
		t.Errorf("fetch err: %v", results[0].Err)
	}
}

func TestDownloaderNewPanicsOnEmptyServers(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("New with zero servers should panic")
		}
	}()
	_ = New(queue.New(), nil, Options{})
}

func TestDownloaderDoubleStart(t *testing.T) {
	ms := newMockNNTP(t)
	d := New(queue.New(), []*Server{testServer(t, "s", ms.addr)}, Options{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	if err := d.Start(t.Context()); !errors.Is(err, ErrAlreadyStarted) {
		t.Errorf("second Start = %v, want ErrAlreadyStarted", err)
	}
}
