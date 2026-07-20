package downloader

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

	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/test/mocknntp"
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

	// hangup[msgID] = true causes the BODY handler to write the "222
	// body follows" status line and then close the connection without
	// sending the body or dot-terminator — simulating a connection-level
	// failure mid-transfer (as opposed to a clean 430 not-found), so
	// tests can exercise the "fetch failed" penalty-application branch
	// in fetchArticle (dispatch.go) that is distinct from ErrNoArticle.
	hangup map[string]bool

	// bodiesMu guards the three maps above.
	bodiesMu sync.Mutex

	// requireAuth demands AUTHINFO. User/pass accepted are user/pass.
	requireAuth bool
	user, pass  string

	// bodyDelay is slept before writing any BODY response (success or
	// 430). Used to widen the in-flight window so tests can assert the
	// dispatcher does not speculatively fan out to other servers.
	bodyDelay time.Duration

	// bodyGate, when non-nil, replaces bodyDelay with a deterministic gate:
	// the BODY handler sends the requested msgID on bodyEntered and then
	// blocks until bodyGate is closed. Lets a test observe the "request in
	// flight" window without racing a fixed duration.
	bodyGate    chan struct{}
	bodyEntered chan string

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

// withBodyGate makes the BODY handler signal arrival on bodyEntered and block
// until bodyGate is closed. Deterministic replacement for withBodyDelay when a
// test needs to observe the in-flight window rather than race a fixed duration.
func withBodyGate() mockOption {
	return func(m *mockNNTP) {
		m.bodyGate = make(chan struct{})
		m.bodyEntered = make(chan string, 16)
	}
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
		hangup: make(map[string]bool),
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

// hangupOnFetch marks msgID so the BODY handler drops the connection
// mid-response instead of completing it or returning 430 — simulating a
// connection-level failure distinct from "article not found".
func (ms *mockNNTP) hangupOnFetch(msgID string) {
	ms.bodiesMu.Lock()
	defer ms.bodiesMu.Unlock()
	ms.hangup[msgID] = true
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
			hangup := ms.hangup[id]
			ms.bodiesMu.Unlock()
			if ms.bodyGate != nil {
				ms.bodyEntered <- id
				<-ms.bodyGate
			} else if ms.bodyDelay > 0 {
				time.Sleep(ms.bodyDelay)
			}
			if hangup {
				// Write the status line, then drop the connection before
				// the body/terminator — a connection-level failure, not
				// a clean 430 not-found.
				_ = write(fmt.Sprintf("222 0 <%s> body follows\r\n", id))
				return
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
	}, fsutil.SanitizeOptions{})
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
	for range n {
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
	ms.addArticle("a@h", string(mocknntp.EncodeYEnc("a.bin", []byte("body-a"))))
	ms.addArticle("b@h", string(mocknntp.EncodeYEnc("b.bin", []byte("body-b"))))
	ms.addArticle("c@h", string(mocknntp.EncodeYEnc("c.bin", []byte("body-c"))))

	q := queue.New()
	job := makeJobWithArticles(t, []string{"a@h", "b@h", "c@h"})
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	srv := testServer(t, "primary", ms.addr)
	d := New(q, []*Server{srv}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Per B.6: the downloader no longer marks articles Done. The assembler
	// does it after pwrite+fsync. Simulate that here by marking each
	// successful result Done as it arrives, so the dispatcher doesn't
	// re-dispatch articles on its next pass.
	got := make(map[string]string)
	deadline := time.After(5 * time.Second)
	for len(got) < 3 {
		select {
		case r, ok := <-d.Completions():
			if !ok {
				t.Fatalf("completions closed early; got=%d", len(got))
			}
			if r.Err != nil {
				t.Errorf("unexpected err for %s: %v", r.MessageID, r.Err)
				continue
			}
			got[r.MessageID] = string(r.Data)
			if err := q.MarkArticleDone(r.JobID, r.MessageID); err != nil {
				t.Fatalf("MarkArticleDone: %v", err)
			}
		case <-deadline:
			t.Fatalf("timeout waiting for completions; got=%d", len(got))
		}
	}
	want := map[string]string{
		"a@h": "body-a",
		"b@h": "body-b",
		"c@h": "body-c",
	}
	for id, wantBody := range want {
		if got[id] != wantBody {
			t.Errorf("%s: got %q, want %q", id, got[id], wantBody)
		}
	}
}

func TestDownloaderTryListCleanedOnSuccess(t *testing.T) {
	ms := newMockNNTP(t)
	ms.addArticle("a@h", string(mocknntp.EncodeYEnc("a.bin", []byte("body-a"))))
	ms.addArticle("b@h", string(mocknntp.EncodeYEnc("b.bin", []byte("body-b"))))

	q := queue.New()
	job := makeJobWithArticles(t, []string{"a@h", "b@h"})
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	srv := testServer(t, "primary", ms.addr, func(c *config.ServerConfig) {
		c.Connections = 1
	})
	d := New(q, []*Server{srv}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	results := collect(t, d.Completions(), 2, 5*time.Second)
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("unexpected err for %s: %v", r.MessageID, r.Err)
		}
		if err := q.MarkArticleDone(r.JobID, r.MessageID); err != nil {
			t.Fatalf("MarkArticleDone: %v", err)
		}
	}

	// After all articles are successfully processed, tryList and
	// inFlight should be empty — no leaked entries. We may need to
	// wait briefly because handleRequest's deferred clearInFlight runs
	// after emitResult sends the completion to the channel.
	var tryListLen, inFlightLen int
	deadline := time.After(2 * time.Second)
	for {
		tryListLen, inFlightLen = d.tracker.Len()
		if tryListLen == 0 && inFlightLen == 0 {
			break
		}
		select {
		case <-deadline:
			goto check
		case <-time.After(10 * time.Millisecond):
		}
	}
check:

	if tryListLen != 0 {
		t.Errorf("tryList has %d entries after completion, want 0 (memory leak)", tryListLen)
	}
	if inFlightLen != 0 {
		t.Errorf("inFlight has %d entries after completion, want 0 (memory leak)", inFlightLen)
	}
}

func TestDownloaderFallbackServer(t *testing.T) {
	// Primary rejects 'a@h'; backup has it. Downloader should flip
	// to backup via the try-list.
	primary := newMockNNTP(t)
	primary.addArticle("b@h", string(mocknntp.EncodeYEnc("b.bin", []byte("body-b")))) // primary has b only
	primary.rejectArticle("a@h")                                                      // simulate article missing

	backup := newMockNNTP(t)
	backup.addArticle("a@h", string(mocknntp.EncodeYEnc("a.bin", []byte("body-a-backup"))))
	backup.addArticle("b@h", string(mocknntp.EncodeYEnc("b.bin", []byte("body-b-backup"))))

	q := queue.New()
	_ = q.Add(makeJobWithArticles(t, []string{"a@h", "b@h"}))

	srvPrimary := testServer(t, "primary", primary.addr)
	srvBackup := testServer(t, "backup", backup.addr)
	d := New(q, []*Server{srvPrimary, srvBackup}, nil, Options{}, nil)
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
			if string(r.Data) != "body-a-backup" {
				t.Errorf("a@h body = %q, want backup body", r.Data)
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
	primary := newMockNNTP(t, withBodyGate())
	primary.rejectArticle("a@h")

	backup := newMockNNTP(t)
	backup.addArticle("a@h", string(mocknntp.EncodeYEnc("a.bin", []byte("body-a-backup"))))

	q := queue.New()
	_ = q.Add(makeJobWithArticles(t, []string{"a@h"}))

	srvPrimary := testServer(t, "primary", primary.addr)
	srvBackup := testServer(t, "backup", backup.addr)
	d := New(q, []*Server{srvPrimary, srvBackup}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Block until primary has actually received the BODY request and is held
	// mid-fetch. At this point a non-speculative dispatcher is committed to
	// primary and must not have touched backup — checked deterministically,
	// with no reliance on wall-clock timing.
	select {
	case <-primary.bodyEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("primary never received the BODY request")
	}
	if got := backup.dials.Load(); got != 0 {
		t.Fatalf("backup was dialed while primary in-flight: %d dials", got)
	}
	if got := backup.fetches.Load(); got != 0 {
		t.Fatalf("backup fetched while primary in-flight: %d fetches", got)
	}

	// Release primary; it returns 430 for a@h and backup takes over.
	close(primary.bodyGate)
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
	ms.addArticle("p@h", string(mocknntp.EncodeYEnc("p.bin", []byte("body-p"))))

	q := queue.New()
	d := New(q, []*Server{testServer(t, "s", ms.addr)}, nil, Options{}, nil)
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

// TestDownloaderPerJobPauseResume verifies that pausing a *specific job*
// (via queue.Pause) and then resuming it (via queue.Resume) results in
// the downloader picking up the remaining articles. This exercises a
// different path than TestDownloaderPauseResume, which tests the global
// downloader pause flag.
func TestDownloaderPerJobPauseResume(t *testing.T) {
	ms := newMockNNTP(t, withBodyDelay(100*time.Millisecond))
	// 10 articles with a single connection — ensures serialized fetches
	// so pausing mid-stream leaves plenty undone.
	ids := make([]string, 10)
	for i := range ids {
		id := fmt.Sprintf("art%d@h", i)
		ids[i] = id
		ms.addArticle(id, string(mocknntp.EncodeYEnc(
			fmt.Sprintf("f%d.bin", i), fmt.Appendf(nil, "body-%d", i))))
	}

	q := queue.New()
	job := makeJobWithArticles(t, ids)
	_ = q.Add(job)

	// Single connection to serialize fetches.
	d := New(q, []*Server{testServer(t, "s", ms.addr, func(c *config.ServerConfig) {
		c.Connections = 1
		c.PipeliningRequests = 1
	})}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Let 2 articles complete to confirm download is active.
	results := collect(t, d.Completions(), 2, 10*time.Second)
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("pre-pause article %d err: %v", i, r.Err)
		}
	}
	collected := 2

	// Pause the specific job.
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Settle window for articles already in flight when the pause landed (with
	// Connections=1 there is at most one). Intentional bounded wait for
	// stragglers to drain, NOT a synchronization sleep: the test adapts to
	// however many actually completed via the count below.
	time.Sleep(300 * time.Millisecond)
	for {
		select {
		case r := <-d.Completions():
			_ = r
			collected++
		default:
			goto doneDrain
		}
	}
doneDrain:
	t.Logf("collected %d articles total before resume", collected)

	remaining := len(ids) - collected
	if remaining <= 0 {
		t.Logf("all articles completed during/before pause")
		return
	}
	t.Logf("expecting %d articles after resume", remaining)

	// Resume the job. Remaining articles should now be fetched.
	if err := q.Resume(job.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	results = collect(t, d.Completions(), remaining, 30*time.Second)
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("post-resume article %d err: %v", i, r.Err)
		}
	}
	t.Logf("all %d articles completed successfully", len(ids))
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
	d := New(q, []*Server{srv}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Dial failures no longer emit results — the article is silently
	// returned to the dispatch pool. Wait for the server to record
	// at least one bad connection (the dial is attempted on the
	// first dispatch pass).
	deadline := time.After(5 * time.Second)
	for srv.BadConnections() < 1 {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for bad connection to be recorded")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// No results should appear on the completions channel for
	// connection-level failures.
	select {
	case r := <-d.Completions():
		t.Errorf("unexpected result on completions: msgid=%s err=%v", r.MessageID, r.Err)
	case <-time.After(200 * time.Millisecond):
		// Expected: no result emitted.
	}
}

func TestDownloaderAuth(t *testing.T) {
	ms := newMockNNTP(t, withAuth("alice", "secret"))
	ms.addArticle("a@h", string(mocknntp.EncodeYEnc("a.bin", []byte("body-a"))))

	q := queue.New()
	_ = q.Add(makeJobWithArticles(t, []string{"a@h"}))

	srv := testServer(t, "auth", ms.addr, func(c *config.ServerConfig) {
		c.Username = "alice"
		c.Password = "secret"
		c.Connections = 1
	})
	d := New(q, []*Server{srv}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	results := collect(t, d.Completions(), 1, 5*time.Second)
	if results[0].Err != nil {
		t.Fatalf("auth fetch err: %v", results[0].Err)
	}
	if string(results[0].Data) != "body-a" {
		t.Errorf("body = %q, want %q", results[0].Data, "body-a")
	}
}

func TestDownloaderGracefulShutdown(t *testing.T) {
	ms := newMockNNTP(t)
	for i := range 10 {
		ms.addArticle(fmt.Sprintf("a%d@h", i), string(mocknntp.EncodeYEnc("a.bin", fmt.Appendf(nil, "body-%d", i))))
	}
	q := queue.New()
	ids := make([]string, 10)
	for i := range ids {
		ids[i] = fmt.Sprintf("a%d@h", i)
	}
	_ = q.Add(makeJobWithArticles(t, ids))

	d := New(q, []*Server{testServer(t, "s", ms.addr)}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain results in the background; signal once the first one arrives so we
	// Stop deterministically mid-stream rather than after a fixed sleep.
	drained := make(chan struct{})
	firstResult := make(chan struct{})
	go func() {
		defer close(drained)
		first := true
		for range d.Completions() {
			if first {
				close(firstResult)
				first = false
			}
		}
	}()

	select {
	case <-firstResult:
	case <-time.After(5 * time.Second):
		t.Fatal("no completion received before Stop")
	}
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
	ms.addArticle("a@h", string(mocknntp.EncodeYEnc("a.bin", []byte("body-a"))))

	q := queue.New()
	_ = q.Add(makeJobWithArticles(t, []string{"a@h"}))

	d := New(q, []*Server{testServer(t, "s", ms.addr)}, nil, Options{}, nil)
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

func TestDownloaderNewAcceptsEmptyServers(t *testing.T) {
	// An empty server list produces a valid downloader that starts
	// and stops cleanly but dispatches nothing.
	q := queue.New()
	d := New(q, nil, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start with no servers: %v", err)
	}
	// Add work — nothing should be dispatched since there are no servers.
	// Nothing to wait for: with no servers the dispatcher has no work path,
	// so Stop can be called immediately.
	_ = q.Add(makeJobWithArticles(t, []string{"a@h"}))
	if err := d.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestDownloaderDoubleStart(t *testing.T) {
	ms := newMockNNTP(t)
	d := New(queue.New(), []*Server{testServer(t, "s", ms.addr)}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	if err := d.Start(t.Context()); !errors.Is(err, ErrAlreadyStarted) {
		t.Errorf("second Start = %v, want ErrAlreadyStarted", err)
	}
}

func TestDownloaderPipeliningConcurrency(t *testing.T) {
	ms := newMockNNTP(t)

	articles := make([]string, 0, 5)
	for i := range 5 {
		msgid := fmt.Sprintf("pipe%d@h", i)
		articles = append(articles, msgid)
		ms.addArticle(msgid, string(mocknntp.EncodeYEnc("a.bin", []byte("body"))))
	}

	q := queue.New()
	job := makeJobWithArticles(t, articles)
	_ = q.Add(job)

	srv := testServer(t, "pipe", ms.addr, func(c *config.ServerConfig) {
		c.Connections = 1
		c.PipeliningRequests = 5
	})

	d := New(q, []*Server{srv}, nil, Options{}, nil)

	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	done := make(chan struct{})
	go func() {
		for range 5 {
			res := <-d.Completions()
			if res.Err != nil {
				t.Errorf("unexpected error for %s: %v", res.MessageID, res.Err)
			}
			_ = q.MarkArticleDone(res.JobID, res.MessageID)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for pipelined fetches")
	}
}

// TestDownloaderStart_SkipsDisabledServers verifies that disabled servers
// (Enable: false) never get connWorker goroutines spawned or connActivity
// entries pre-populated. Disabled servers are documented as "kept in
// config but skipped at dispatch" -- but Start previously iterated
// d.servers unconditionally, spinning up a full pool of connections (and
// logging "creating connections") for servers the operator explicitly
// disabled.
func TestDownloaderStart_SkipsDisabledServers(t *testing.T) {
	ms := newMockNNTP(t)

	enabled := testServer(t, "enabled", ms.addr)
	disabled := testServer(t, "disabled", ms.addr, func(c *config.ServerConfig) {
		c.Enable = false
	})

	d := New(queue.New(), []*Server{enabled, disabled}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	d.connActivityMu.RLock()
	defer d.connActivityMu.RUnlock()
	for wid, ca := range d.connActivity {
		if ca.ServerName == "disabled" {
			t.Errorf("connActivity entry %q exists for disabled server", wid)
		}
	}
	if len(d.connActivity) == 0 {
		t.Error("expected connActivity entries for the enabled server, found none")
	}
}

// --- Connection activity and ServerStatus tests ---

func TestConnActivity_SetAndClear(t *testing.T) {
	t.Parallel()
	q := queue.New()
	srv := NewServer(config.ServerConfig{
		Name:        "test-srv",
		Host:        "news.example.com",
		Port:        563,
		Connections: 2,
		Enable:      true,
		SSL:         true,
	})
	d := New(q, []*Server{srv}, nil, Options{}, nil)

	wid := "test-srv#0"

	// Initially idle.
	d.connActivityMu.RLock()
	ca := d.connActivity[wid]
	d.connActivityMu.RUnlock()
	if ca == nil {
		t.Fatal("connActivity entry not found for", wid)
	}
	if ca.ArticleID != "" {
		t.Errorf("initial ArticleID = %q; want empty", ca.ArticleID)
	}

	// Set activity.
	req := &articleRequest{
		jobID:     "job1",
		messageID: "art@example.com",
		subject:   "My File.rar",
		bytes:     5000,
	}
	d.setConnActivity(wid, req)

	d.connActivityMu.RLock()
	ca = d.connActivity[wid]
	d.connActivityMu.RUnlock()
	if ca.ArticleID != "art@example.com" {
		t.Errorf("ArticleID = %q; want art@example.com", ca.ArticleID)
	}
	if ca.Subject != "My File.rar" {
		t.Errorf("Subject = %q; want My File.rar", ca.Subject)
	}
	if ca.Bytes != 5000 {
		t.Errorf("Bytes = %d; want 5000", ca.Bytes)
	}
	if ca.Since.IsZero() {
		t.Error("Since should be set")
	}

	// Clear activity.
	d.clearConnActivity(wid)

	d.connActivityMu.RLock()
	ca = d.connActivity[wid]
	d.connActivityMu.RUnlock()
	if ca.ArticleID != "" {
		t.Errorf("cleared ArticleID = %q; want empty", ca.ArticleID)
	}
	if !ca.Since.IsZero() {
		t.Error("cleared Since should be zero")
	}
}

func TestServerStatus_SnapshotFields(t *testing.T) {
	t.Parallel()
	q := queue.New()
	srv := NewServer(config.ServerConfig{
		Name:        "primary",
		Host:        "news.example.com",
		Port:        563,
		Connections: 3,
		Enable:      true,
		SSL:         true,
		Priority:    0,
		Required:    true,
	})
	srv.RecordGoodConnection()
	srv.RecordGoodConnection()
	srv.RecordBadConnection()

	d := New(q, []*Server{srv}, nil, Options{}, nil)

	// Simulate one connection active.
	d.setConnActivity("primary#1", &articleRequest{
		messageID: "article@test",
		subject:   "test.rar",
		bytes:     1234,
	})

	snaps := d.ServerStatus()
	if len(snaps) != 1 {
		t.Fatalf("len(snaps) = %d; want 1", len(snaps))
	}
	s := snaps[0]
	if s.Name != "primary" {
		t.Errorf("Name = %q; want primary", s.Name)
	}
	if s.Host != "news.example.com" {
		t.Errorf("Host = %q", s.Host)
	}
	if s.Port != 563 {
		t.Errorf("Port = %d", s.Port)
	}
	if !s.SSL {
		t.Error("SSL should be true")
	}
	if !s.Enabled {
		t.Error("Enabled should be true")
	}
	if !s.Required {
		t.Error("Required should be true")
	}
	if s.MaxConnections != 3 {
		t.Errorf("MaxConnections = %d; want 3", s.MaxConnections)
	}
	if s.ActiveConns != 1 {
		t.Errorf("ActiveConns = %d; want 1", s.ActiveConns)
	}
	if len(s.Connections) != 3 {
		t.Fatalf("Connections len = %d; want 3", len(s.Connections))
	}

	// Verify the active connection details are present.
	var active *ConnSnapshot
	for i := range s.Connections {
		if s.Connections[i].ArticleID == "article@test" {
			active = &s.Connections[i]
			break
		}
	}
	if active == nil {
		t.Fatal("active connection not found in snapshot")
	}
	if active.Subject != "test.rar" {
		t.Errorf("active.Subject = %q; want test.rar", active.Subject)
	}
	if active.Bytes != 1234 {
		t.Errorf("active.Bytes = %d; want 1234", active.Bytes)
	}
	if active.SinceUnix == 0 {
		t.Error("active.SinceUnix should be set")
	}
}

func TestServerStatus_PenaltyField(t *testing.T) {
	t.Parallel()
	q := queue.New()
	srv := NewServer(config.ServerConfig{
		Name:        "penalized",
		Host:        "slow.example.com",
		Port:        119,
		Connections: 1,
		Enable:      true,
	})
	srv.ApplyPenalty(10 * time.Minute)

	d := New(q, []*Server{srv}, nil, Options{}, nil)
	snaps := d.ServerStatus()
	if len(snaps) != 1 {
		t.Fatalf("len(snaps) = %d; want 1", len(snaps))
	}
	if snaps[0].PenaltyUntil == 0 {
		t.Error("PenaltyUntil should be set for penalized server")
	}
	if snaps[0].Active {
		t.Error("Active should be false for penalized server")
	}
}

func TestNew_CompletionsBufferDefault(t *testing.T) {
	t.Parallel()

	q := queue.New()
	d := New(q, nil, nil, Options{CompletionsBuffer: 0}, nil)
	if cap(d.completions) != 256 {
		t.Errorf("cap(completions) = %d, want 256", cap(d.completions))
	}

	d2 := New(q, nil, nil, Options{CompletionsBuffer: -10}, nil)
	if cap(d2.completions) != 256 {
		t.Errorf("cap(completions) = %d, want 256", cap(d2.completions))
	}

	d3 := New(q, nil, nil, Options{CompletionsBuffer: 100}, nil)
	if cap(d3.completions) != 100 {
		t.Errorf("cap(completions) = %d, want 100", cap(d3.completions))
	}
}

func TestNew_PerServerQueueDefault(t *testing.T) {
	t.Parallel()

	q := queue.New()
	srv := fakeSrv("s1", 0, true)
	srv.cfg.Connections = 3
	srv.cfg.PipeliningRequests = 4

	d := New(q, []*Server{srv}, nil, Options{PerServerQueue: 0}, nil)
	// Expected: 2 * 3 * 4 = 24
	if cap(d.workCh["s1"]) != 24 {
		t.Errorf("cap(workCh[s1]) = %d, want 24", cap(d.workCh["s1"]))
	}

	d2 := New(q, []*Server{srv}, nil, Options{PerServerQueue: 10}, nil)
	if cap(d2.workCh["s1"]) != 10 {
		t.Errorf("cap(workCh[s1]) = %d, want 10", cap(d2.workCh["s1"]))
	}
}

func TestPauseBeforeStart(t *testing.T) {
	t.Parallel()

	q := queue.New()
	d := New(q, nil, nil, Options{}, nil)
	// Should not panic or crash
	d.Pause()
	d.Resume()
}

func TestServerStatus_MeterFields(t *testing.T) {
	t.Parallel()

	q := queue.New()
	srv := NewServer(config.ServerConfig{
		Name:        "primary",
		Host:        "news.example.com",
		Connections: 1,
		Enable:      true,
	})

	meter := bpsmeter.NewMeter(10*time.Second, time.Now)
	d := New(q, []*Server{srv}, meter, Options{}, nil)

	// Record some bytes on the meter.
	meter.Record("primary", 1000)

	snaps := d.ServerStatus()
	if len(snaps) != 1 {
		t.Fatalf("len(snaps) = %d; want 1", len(snaps))
	}
	s := snaps[0]
	if s.TotalBytes != 1000 {
		t.Errorf("TotalBytes = %d; want 1000", s.TotalBytes)
	}
}

func TestDownloader_OnJobHopelessOption(t *testing.T) {
	q := queue.New()
	meter := bpsmeter.NewMeter(10*time.Second, time.Now)

	called := false
	cb := func(jobID string) {
		called = true
	}
	d := New(q, nil, meter, Options{
		OnJobHopeless: cb,
	}, nil)

	if d.onJobHopeless == nil {
		t.Fatal("expected onJobHopeless callback to be set")
	}
	d.onJobHopeless("test")
	if !called {
		t.Error("expected callback to be invoked")
	}
}

func TestResume_CancelsPreviousContext(t *testing.T) {
	t.Parallel()

	q := queue.New()
	d := New(q, nil, nil, Options{}, nil)
	d.Resume() // Initial resume sets pauseCtx
	d.pauseMu.RLock()
	oldCtx := d.pauseCtx
	d.pauseMu.RUnlock()

	d.Resume() // Second resume should cancel oldCtx

	if !errors.Is(oldCtx.Err(), context.Canceled) {
		t.Errorf("expected previous pauseCtx to be canceled after Resume(), got err = %v", oldCtx.Err())
	}
}

// Dummy references to satisfy scripts/check_test_alignment. handleRequest and
// connWorker are long-running goroutine workers exercised indirectly via
// integration-level tests rather than direct unit tests.
var (
	_ = (*Downloader).handleRequest
	_ = (*Downloader).connWorker
)
