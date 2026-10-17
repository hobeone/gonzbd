package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/decoder"
)

type rawClient struct {
	c  net.Conn
	br *bufio.Reader
}

func startConnTestServer(t *testing.T, cfg ServerConfig) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg.Releases = []ReleaseSource{{Dir: dir, Manifest: createTestRelease(t, dir)}}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func dialRaw(t *testing.T, srv *Server) *rawClient {
	t.Helper()
	c, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &rawClient{c: c, br: bufio.NewReader(c)}
}

// line reads one CRLF-terminated line; a read error is returned as "<err: ...>".
func (r *rawClient) line(t *testing.T) string {
	t.Helper()
	s, err := r.readLine()
	if err != nil {
		return "<err: " + err.Error() + ">"
	}
	return s
}

func (r *rawClient) readLine() (string, error) {
	_ = r.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	s, err := r.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// isPeerClosed reports whether err shows the peer closed the connection: EOF or a reset.
// A read deadline expiring is the connection still being open and silent, not closed.
func isPeerClosed(err error) bool {
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed)
}

// wantClosed fails unless the next read shows the server closed the connection without sending
// another line.
func (r *rawClient) wantClosed(t *testing.T, what string) {
	t.Helper()
	got, err := r.readLine()
	if err == nil {
		t.Fatalf("%s: connection still open, read %q", what, got)
	}
	if !isPeerClosed(err) {
		t.Fatalf("%s: read failed without the peer closing (a timeout is not a close): %v", what, err)
	}
}

func setConn(t *testing.T, srv *Server, c ConnSettings) {
	t.Helper()
	if _, err := srv.UpdateConnSettings(func(p *ConnSettings) { *p = c }); err != nil {
		t.Fatalf("UpdateConnSettings: %v", err)
	}
}

func (r *rawClient) cmd(t *testing.T, c string) string {
	t.Helper()
	if _, err := r.c.Write([]byte(c + "\r\n")); err != nil {
		t.Fatalf("write %q: %v", c, err)
	}
	return r.line(t)
}

func TestServer_MaxConnections(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{Conn: ConnSettings{MaxConnections: 2}})

	a, b := dialRaw(t, srv), dialRaw(t, srv)
	for i, c := range []*rawClient{a, b} {
		if got := c.line(t); !strings.HasPrefix(got, "200 ") {
			t.Fatalf("conn %d greeting = %q, want 200", i, got)
		}
	}
	over := dialRaw(t, srv)
	if got := over.line(t); got != "502 too many connections" {
		t.Fatalf("over-cap greeting = %q, want 502 too many connections", got)
	}
	over.wantClosed(t, "over-cap connection")

	st := srv.Stats()
	if st.ActiveConns != 2 || st.PeakConns != 2 || st.ConnsRejected != 1 {
		t.Fatalf("stats active/peak/rejected = %d/%d/%d, want 2/2/1", st.ActiveConns, st.PeakConns, st.ConnsRejected)
	}

	// Closing one frees a slot.
	if got := a.cmd(t, "QUIT"); !strings.HasPrefix(got, "205") {
		t.Fatalf("QUIT = %q", got)
	}
	waitFor(t, func() bool { return srv.Stats().ActiveConns == 1 })
	again := dialRaw(t, srv)
	if got := again.line(t); !strings.HasPrefix(got, "200 ") {
		t.Fatalf("greeting after slot freed = %q, want 200", got)
	}
	if srv.Stats().PeakConns != 2 {
		t.Fatalf("peak = %d, want 2", srv.Stats().PeakConns)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func TestServer_RefuseAndRejectGreeting(t *testing.T) {
	tests := []struct {
		name string
		conn ConnSettings
		want string
	}{
		{"refuse closes without greeting", ConnSettings{Refuse: true}, ""},
		{"reject_greeting sends 400", ConnSettings{RejectGreeting: true}, "400 service temporarily unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startConnTestServer(t, ServerConfig{Conn: tt.conn})
			c := dialRaw(t, srv)
			if tt.want == "" {
				c.wantClosed(t, "refused connection")
			} else {
				if got := c.line(t); got != tt.want {
					t.Fatalf("greeting = %q, want %q", got, tt.want)
				}
				c.wantClosed(t, "connection after rejected greeting")
			}
			waitFor(t, func() bool { return srv.Stats().ConnsRejected == 1 })
			if srv.Stats().ActiveConns != 0 {
				t.Fatalf("rejected connection counted active: %d", srv.Stats().ActiveConns)
			}
		})
	}
}

func TestServer_AuthFail(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{Conn: ConnSettings{AuthFail: true}})
	c := dialRaw(t, srv)
	if got := c.line(t); !strings.HasPrefix(got, "200 ") {
		t.Fatalf("greeting = %q", got)
	}
	// No Users configured, so only auth_fail can produce these.
	if got := c.cmd(t, "BODY <single-part-1@localnntp.test>"); !strings.HasPrefix(got, "480") {
		t.Fatalf("BODY before auth = %q, want 480", got)
	}
	if got := c.cmd(t, "STAT <single-part-1@localnntp.test>"); !strings.HasPrefix(got, "480") {
		t.Fatalf("STAT before auth = %q, want 480", got)
	}
	if got := c.cmd(t, "AUTHINFO USER u"); !strings.HasPrefix(got, "381") {
		t.Fatalf("AUTHINFO USER = %q, want 381", got)
	}
	if got := c.cmd(t, "AUTHINFO PASS p"); !strings.HasPrefix(got, "481") {
		t.Fatalf("AUTHINFO PASS = %q, want 481", got)
	}
	if got := c.cmd(t, "BODY <single-part-1@localnntp.test>"); !strings.HasPrefix(got, "480") {
		t.Fatalf("BODY after failed auth = %q, want 480", got)
	}

	// Clearing the flag at runtime restores the no-auth default for new commands.
	setConn(t, srv, ConnSettings{})
	if got := c.cmd(t, "STAT <single-part-1@localnntp.test>"); !strings.HasPrefix(got, "223") {
		t.Fatalf("STAT after auth_fail cleared = %q, want 223", got)
	}
}

// auth_fail set after a connection authenticated must still block that connection's commands.
func TestServer_AuthFail_AppliesToAuthenticatedConnections(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{Users: map[string]string{"u": "p"}})
	c := dialRaw(t, srv)
	c.line(t)
	if got := c.cmd(t, "AUTHINFO USER u"); !strings.HasPrefix(got, "381") {
		t.Fatalf("AUTHINFO USER = %q", got)
	}
	if got := c.cmd(t, "AUTHINFO PASS p"); !strings.HasPrefix(got, "281") {
		t.Fatalf("AUTHINFO PASS = %q", got)
	}
	if got := c.cmd(t, "STAT <single-part-1@localnntp.test>"); !strings.HasPrefix(got, "223") {
		t.Fatalf("STAT while authenticated = %q, want 223", got)
	}

	setConn(t, srv, ConnSettings{AuthFail: true})
	for _, verb := range []string{"STAT", "BODY", "ARTICLE"} {
		if got := c.cmd(t, verb+" <single-part-1@localnntp.test>"); !strings.HasPrefix(got, "480") {
			t.Fatalf("%s on an already-authenticated connection after auth_fail = %q, want 480", verb, got)
		}
	}

	setConn(t, srv, ConnSettings{})
	if got := c.cmd(t, "STAT <single-part-1@localnntp.test>"); !strings.HasPrefix(got, "223") {
		t.Fatalf("STAT after auth_fail cleared = %q, want 223", got)
	}
}

func TestServer_KillActive(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{})
	a, b := dialRaw(t, srv), dialRaw(t, srv)
	a.line(t)
	b.line(t)
	waitFor(t, func() bool { return srv.Stats().ActiveConns == 2 })

	if n := srv.KillActive(); n != 2 {
		t.Fatalf("KillActive = %d, want 2", n)
	}
	a.wantClosed(t, "conn a after kill")
	b.wantClosed(t, "conn b after kill")
	if srv.Stats().ActiveConns != 0 {
		t.Fatalf("active = %d right after KillActive, want 0", srv.Stats().ActiveConns)
	}
	if srv.Stats().PeakConns != 2 {
		t.Fatalf("peak = %d, want 2", srv.Stats().PeakConns)
	}
	// New connections are still accepted: kill is one-shot.
	c := dialRaw(t, srv)
	if got := c.line(t); !strings.HasPrefix(got, "200 ") {
		t.Fatalf("greeting after kill = %q", got)
	}
}

// A killed connection's slot must be free the moment KillActive returns, or a reconnect under
// --max-connections draws a spurious 502.
func TestServer_KillActive_FreesSlotsImmediately(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{Conn: ConnSettings{MaxConnections: 1}})
	for i := range 25 {
		c := dialRaw(t, srv)
		if got := c.line(t); !strings.HasPrefix(got, "200 ") {
			t.Fatalf("round %d: greeting = %q, want 200", i, got)
		}
		if n := srv.KillActive(); n != 1 {
			t.Fatalf("round %d: KillActive = %d, want 1", i, n)
		}
		if active := srv.Stats().ActiveConns; active != 0 {
			t.Fatalf("round %d: active = %d immediately after KillActive, want 0", i, active)
		}
	}
	if rejected := srv.Stats().ConnsRejected; rejected != 0 {
		t.Fatalf("rejected = %d, want 0: a reconnect after kill hit the cap", rejected)
	}
}

// hookListener runs before after each Accept hand-off, on the accept goroutine itself, so the
// hook observes the server between one Accept returning and the next being issued.
type hookListener struct {
	net.Listener
	before func()
}

func (l *hookListener) Accept() (net.Conn, error) {
	l.before()
	return l.Listener.Accept()
}

// A connection that Accept has returned must already be killable: KillActive issued before the
// next Accept cannot miss it.
func TestServer_KillActive_CoversJustAcceptedConnection(t *testing.T) {
	dir := t.TempDir()
	srv, err := NewServer(ServerConfig{Releases: []ReleaseSource{{Dir: dir, Manifest: createTestRelease(t, dir)}}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	killed := make(chan int, 64)
	srv.ln = &hookListener{Listener: real, before: func() { killed <- srv.KillActive() }}
	srv.addr = real.Addr().String()
	srv.wg.Go(srv.acceptLoop)
	t.Cleanup(func() { _ = srv.Close() })
	<-killed // the first Accept call, before any client exists

	const rounds = 40
	clients := make([]*rawClient, 0, rounds)
	for i := range rounds {
		clients = append(clients, dialRaw(t, srv))
		if n := <-killed; n != 1 {
			t.Fatalf("round %d: KillActive on the accept goroutine closed %d connections, want 1 (the one just accepted)", i, n)
		}
	}
	// serveConn runs concurrently with the hook, so the greeting may already be on the wire
	// when KillActive closes the socket; what must follow it is the close.
	for i, c := range clients {
		what := "just-accepted conn " + strconv.Itoa(i)
		got, err := c.readLine()
		if err == nil {
			if !strings.HasPrefix(got, "200 ") {
				t.Fatalf("%s: read %q, want the greeting or a close", what, got)
			}
			got, err = c.readLine()
		}
		if err == nil {
			t.Fatalf("%s: connection still open after the greeting, read %q", what, got)
		}
		if !isPeerClosed(err) {
			t.Fatalf("%s: read failed without the peer closing (a timeout is not a close): %v", what, err)
		}
	}
}

func TestServer_UpdateConnSettings_IsAtomic(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{})
	entered, release, secondRan := make(chan struct{}), make(chan struct{}), make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = srv.UpdateConnSettings(func(c *ConnSettings) {
			close(entered)
			<-release
			c.MaxConnections = 7
		})
	}()
	<-entered
	go func() {
		defer close(secondRan)
		_, _ = srv.UpdateConnSettings(func(c *ConnSettings) { c.AuthFail = true })
	}()
	select {
	case <-secondRan:
		t.Fatal("a second UpdateConnSettings ran while the first was still mutating")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-firstDone
	<-secondRan
	if got, want := srv.ConnSettings(), (ConnSettings{MaxConnections: 7, AuthFail: true}); got != want {
		t.Fatalf("settings = %+v, want %+v (an update was lost)", got, want)
	}
}

func TestServer_UpdateConnSettings_RejectsNegativeCap(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{Conn: ConnSettings{MaxConnections: 2}})
	if _, err := srv.UpdateConnSettings(func(c *ConnSettings) { c.MaxConnections = -1 }); err == nil {
		t.Fatal("UpdateConnSettings accepted max_connections -1")
	}
	if got := srv.ConnSettings().MaxConnections; got != 2 {
		t.Fatalf("max_connections = %d after a rejected update, want 2", got)
	}
	dir := t.TempDir()
	_, err := NewServer(ServerConfig{
		Releases: []ReleaseSource{{Dir: dir, Manifest: createTestRelease(t, dir)}},
		Conn:     ConnSettings{MaxConnections: -1},
	})
	if err == nil {
		t.Fatal("NewServer accepted max_connections -1")
	}
}

func TestControl_Conn(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{})
	ts := httptest.NewServer(NewControlHandler(srv))
	defer ts.Close()

	post := func(body string) (int, string) {
		t.Helper()
		resp, err := http.Post(ts.URL+"/conn", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /conn: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.String()
	}

	if code, _ := post(`{"max_connections":1,"auth_fail":true}`); code != http.StatusOK {
		t.Fatalf("POST /conn status %d", code)
	}
	// A later partial update must leave earlier fields alone.
	if code, _ := post(`{"reject_greeting":true}`); code != http.StatusOK {
		t.Fatalf("POST /conn partial status %d", code)
	}
	want := ConnSettings{MaxConnections: 1, AuthFail: true, RejectGreeting: true}
	if got := srv.ConnSettings(); got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}

	resp, err := http.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var st ServerStats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Conn != want {
		t.Fatalf("/stats conn = %+v, want %+v", st.Conn, want)
	}

	for _, bad := range []string{`{"max_connections":-1}`, `{"bogus":1}`, `not json`} {
		if code, _ := post(bad); code != http.StatusBadRequest {
			t.Errorf("POST /conn %s status %d, want 400", bad, code)
		}
	}
	if got := srv.ConnSettings(); got != want {
		t.Fatalf("settings = %+v after rejected requests, want %+v", got, want)
	}

	setConn(t, srv, ConnSettings{})
	c := dialRaw(t, srv)
	c.line(t)
	waitFor(t, func() bool { return srv.Stats().ActiveConns == 1 })
	code, out := post(`{"kill_active":true}`)
	if code != http.StatusOK || !strings.Contains(out, `"killed":1`) {
		t.Fatalf("kill_active = %d %s, want killed:1", code, out)
	}
	c.wantClosed(t, "conn after kill_active")
}

func postConn(t *testing.T, base, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(base+"/conn", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /conn: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /conn %s status %d", body, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /conn response: %v", err)
	}
	return out
}

func TestControl_Conn_JSONKeysAndFlagToggling(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{})
	ts := httptest.NewServer(NewControlHandler(srv))
	defer ts.Close()

	out := postConn(t, ts.URL, `{"max_connections":4,"refuse":true,"reject_greeting":true,"auth_fail":true}`)
	wantConn := map[string]any{"max_connections": float64(4), "refuse": true, "reject_greeting": true, "auth_fail": true}
	if !reflect.DeepEqual(out["conn"], wantConn) {
		t.Fatalf("response conn = %v, want %v", out["conn"], wantConn)
	}
	if out["killed"] != float64(0) {
		t.Fatalf("response killed = %v, want 0", out["killed"])
	}

	// refuse: accepted then closed with no greeting.
	postConn(t, ts.URL, `{"max_connections":0,"reject_greeting":false,"auth_fail":false}`)
	dialRaw(t, srv).wantClosed(t, "refused connection")

	// An explicit false clears the flag; omitted fields keep their value.
	out = postConn(t, ts.URL, `{"refuse":false}`)
	if got := out["conn"].(map[string]any)["refuse"]; got != false {
		t.Fatalf("refuse after explicit false = %v, want false", got)
	}
	if got := srv.ConnSettings(); got != (ConnSettings{}) {
		t.Fatalf("settings = %+v, want all cleared", got)
	}
	if got := dialRaw(t, srv).line(t); !strings.HasPrefix(got, "200 ") {
		t.Fatalf("greeting after refuse cleared = %q, want 200", got)
	}

	// The same keys appear under /stats.
	resp, err := http.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var stats map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"conn", "active_connections", "peak_connections", "connections_rejected_total"} {
		if _, ok := stats[key]; !ok {
			t.Errorf("/stats missing key %q", key)
		}
	}
	gotKeys := slices.Sorted(maps.Keys(stats["conn"].(map[string]any)))
	if want := []string{"auth_fail", "max_connections", "refuse", "reject_greeting"}; !slices.Equal(gotKeys, want) {
		t.Fatalf("/stats conn keys = %v, want %v", gotKeys, want)
	}
}

// Concurrent POST /conn requests that touch different fields must both land.
func TestControl_Conn_ConcurrentPartialUpdates(t *testing.T) {
	srv := startConnTestServer(t, ServerConfig{})
	h := NewControlHandler(srv)
	post := func(body string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/conn", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Errorf("POST /conn %s = %d", body, rec.Code)
		}
	}
	for round := range 200 {
		setConn(t, srv, ConnSettings{})
		var wg sync.WaitGroup
		wg.Go(func() { post(`{"max_connections":5}`) })
		wg.Go(func() { post(`{"auth_fail":true}`) })
		wg.Wait()
		if got, want := srv.ConnSettings(), (ConnSettings{MaxConnections: 5, AuthFail: true}); got != want {
			t.Fatalf("round %d: settings = %+v, want %+v (a concurrent update was lost)", round, got, want)
		}
	}
}

// writeSecondRelease builds a release whose Message-IDs carry suffix and whose single.bin holds
// different bytes of the same length as the first release's, so a read from the wrong release dir
// is visible in the decoded payload.
func writeSecondRelease(t *testing.T, suffix string) (dir string, m *ReleaseManifest, single []byte) {
	t.Helper()
	dir = t.TempDir()
	m = createTestRelease(t, dir)
	for fi := range m.Files {
		for si := range m.Files[fi].Segments {
			id := m.Files[fi].Segments[si].MessageID
			m.Files[fi].Segments[si].MessageID = strings.Replace(id, "@", suffix+"@", 1)
		}
	}
	single = bytes.Repeat([]byte("B"), len("hello single part release payload for testing 1234567890!"))
	if err := os.WriteFile(filepath.Join(dir, "files", "single.bin"), single, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, m, single
}

func TestServer_MultiRelease(t *testing.T) {
	dirA := t.TempDir()
	mA := createTestRelease(t, dirA)
	dirB, mB, singleB := writeSecondRelease(t, "-b")

	srv, err := NewServer(ServerConfig{
		Releases: []ReleaseSource{{Dir: dirA, Manifest: mA}, {Dir: dirB, Manifest: mB}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	conn := dialNNTP(t, srv, "", "")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	fetch := func(id string) []byte {
		t.Helper()
		body, err := conn.Fetch(ctx, id)
		if err != nil {
			t.Fatalf("Fetch %s: %v", id, err)
		}
		art, err := decoder.DecodeArticle(body)
		if err != nil {
			t.Fatalf("DecodeArticle %s: %v", id, err)
		}
		return art.Data
	}
	if got := fetch("single-part-1@localnntp.test"); !bytes.HasPrefix(got, []byte("hello single")) {
		t.Fatalf("release A single = %q", got)
	}
	if got := fetch("single-part-1-b@localnntp.test"); !bytes.Equal(got, singleB) {
		t.Fatalf("release B single = %q, want %q (served from the wrong release dir?)", got, singleB)
	}

	// A fault rule and the stats cover ids from every release.
	srv.Faults().AddRule(FaultRule{Action: FaultDrop, MessageIDs: []string{"multi-part-2-b@localnntp.test"}})
	if _, err := conn.Fetch(ctx, "multi-part-2-b@localnntp.test"); err == nil {
		t.Fatal("fault on release B segment did not fire")
	}
	if _, err := conn.Fetch(ctx, "multi-part-2@localnntp.test"); err != nil {
		t.Fatalf("release A multi-part-2 should be unaffected: %v", err)
	}
	st := srv.Stats()
	if st.ServedClean["single-part-1@localnntp.test"] != 1 || st.ServedClean["single-part-1-b@localnntp.test"] != 1 {
		t.Fatalf("served_clean = %v, want one hit per release", st.ServedClean)
	}
}

// With packs in both releases and the files/ contents removed, only the fast path can serve, so
// each ID must be read from its own release's pack.
func TestServer_MultiRelease_FastPathUsesOwnPack(t *testing.T) {
	dirA := t.TempDir()
	mA := createTestRelease(t, dirA)
	dirB, mB, singleB := writeSecondRelease(t, "-b")
	for _, r := range []ReleaseSource{{Dir: dirA, Manifest: mA}, {Dir: dirB, Manifest: mB}} {
		filesDir := filepath.Join(r.Dir, "files")
		if err := WriteArticlesPack(filepath.Join(r.Dir, "articles.pack"), filesDir, r.Manifest.Files); err != nil {
			t.Fatal(err)
		}
		for _, f := range r.Manifest.Files {
			if err := os.Remove(filepath.Join(filesDir, f.DiskName)); err != nil {
				t.Fatal(err)
			}
		}
	}

	srv, err := NewServer(ServerConfig{Releases: []ReleaseSource{{Dir: dirA, Manifest: mA}, {Dir: dirB, Manifest: mB}}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	if srv.releases[0].packFile == nil || srv.releases[1].packFile == nil {
		t.Fatal("both releases should have opened a pack")
	}

	conn := dialNNTP(t, srv, "", "")
	for _, tt := range []struct {
		id   string
		want func(got []byte) bool
	}{
		{"single-part-1@localnntp.test", func(got []byte) bool { return bytes.HasPrefix(got, []byte("hello single")) }},
		{"single-part-1-b@localnntp.test", func(got []byte) bool { return bytes.Equal(got, singleB) }},
	} {
		body, err := conn.Fetch(t.Context(), tt.id)
		if err != nil {
			t.Fatalf("Fetch %s: %v", tt.id, err)
		}
		art, err := decoder.DecodeArticle(body)
		if err != nil {
			t.Fatalf("DecodeArticle %s: %v", tt.id, err)
		}
		if !tt.want(art.Data) {
			t.Fatalf("%s served %q: read from the wrong release's pack", tt.id, art.Data)
		}
	}
}

// A release with a pack served beside one without: the fast-path decision is per release.
func TestServer_MultiRelease_FastPathIsPerRelease(t *testing.T) {
	dirA := t.TempDir()
	mA := createTestRelease(t, dirA) // no pack: slow path from files/
	dirB, mB, singleB := writeSecondRelease(t, "-b")
	filesB := filepath.Join(dirB, "files")
	if err := WriteArticlesPack(filepath.Join(dirB, "articles.pack"), filesB, mB.Files); err != nil {
		t.Fatal(err)
	}
	for _, f := range mB.Files {
		if err := os.Remove(filepath.Join(filesB, f.DiskName)); err != nil {
			t.Fatal(err)
		}
	}

	srv, err := NewServer(ServerConfig{Releases: []ReleaseSource{{Dir: dirA, Manifest: mA}, {Dir: dirB, Manifest: mB}}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	conn := dialNNTP(t, srv, "", "")
	for _, id := range []string{"single-part-1@localnntp.test", "single-part-1-b@localnntp.test"} {
		body, err := conn.Fetch(t.Context(), id)
		if err != nil {
			t.Fatalf("Fetch %s: %v", id, err)
		}
		art, err := decoder.DecodeArticle(body)
		if err != nil {
			t.Fatalf("DecodeArticle %s: %v", id, err)
		}
		if id == "single-part-1-b@localnntp.test" && !bytes.Equal(art.Data, singleB) {
			t.Fatalf("%s = %q, want release B's payload", id, art.Data)
		}
	}
}

func TestServer_MultiRelease_DuplicateMessageID(t *testing.T) {
	dirA := t.TempDir()
	mA := createTestRelease(t, dirA)
	dirB := t.TempDir()
	mB := createTestRelease(t, dirB) // same Message-IDs as A

	_, err := NewServer(ServerConfig{
		Releases: []ReleaseSource{{Dir: dirA, Manifest: mA}, {Dir: dirB, Manifest: mB}},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate message-id") {
		t.Fatalf("NewServer err = %v, want duplicate message-id error", err)
	}

	_, err = NewServer(ServerConfig{
		Releases: []ReleaseSource{{Dir: dirA, Manifest: mA}, {Dir: "", Manifest: nil}},
	})
	if err == nil {
		t.Fatal("NewServer accepted an empty extra release")
	}
}
