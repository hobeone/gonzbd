package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	cfg.ReleaseDir = dir
	cfg.Manifest = createTestRelease(t, dir)
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

// line reads one CRLF-terminated line; a read error (including EOF) is returned as "<err: ...>".
func (r *rawClient) line(t *testing.T) string {
	t.Helper()
	_ = r.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	s, err := r.br.ReadString('\n')
	if err != nil {
		return "<err: " + err.Error() + ">"
	}
	return strings.TrimRight(s, "\r\n")
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
	if got := over.line(t); !strings.HasPrefix(got, "<err: ") {
		t.Fatalf("over-cap connection not closed, read %q", got)
	}

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
			got := c.line(t)
			if tt.want == "" {
				if !strings.HasPrefix(got, "<err: ") {
					t.Fatalf("refused connection sent %q, want bare close", got)
				}
			} else {
				if got != tt.want {
					t.Fatalf("greeting = %q, want %q", got, tt.want)
				}
				if next := c.line(t); !strings.HasPrefix(next, "<err: ") {
					t.Fatalf("connection not closed after greeting, read %q", next)
				}
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
	srv.SetConnSettings(ConnSettings{})
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
	for i, c := range []*rawClient{a, b} {
		if got := c.line(t); !strings.HasPrefix(got, "<err: ") {
			t.Fatalf("conn %d still alive after kill, read %q", i, got)
		}
	}
	waitFor(t, func() bool { return srv.Stats().ActiveConns == 0 })
	if srv.Stats().PeakConns != 2 {
		t.Fatalf("peak = %d, want 2", srv.Stats().PeakConns)
	}
	// New connections are still accepted: kill is one-shot.
	c := dialRaw(t, srv)
	if got := c.line(t); !strings.HasPrefix(got, "200 ") {
		t.Fatalf("greeting after kill = %q", got)
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

	srv.SetConnSettings(ConnSettings{})
	c := dialRaw(t, srv)
	c.line(t)
	waitFor(t, func() bool { return srv.Stats().ActiveConns == 1 })
	code, out := post(`{"kill_active":true}`)
	if code != http.StatusOK || !strings.Contains(out, `"killed":1`) {
		t.Fatalf("kill_active = %d %s, want killed:1", code, out)
	}
	if got := c.line(t); !strings.HasPrefix(got, "<err: ") {
		t.Fatalf("conn alive after kill_active, read %q", got)
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
		ReleaseDir:    dirA,
		Manifest:      mA,
		ExtraReleases: []ReleaseSource{{Dir: dirB, Manifest: mB}},
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

func TestServer_MultiRelease_DuplicateMessageID(t *testing.T) {
	dirA := t.TempDir()
	mA := createTestRelease(t, dirA)
	dirB := t.TempDir()
	mB := createTestRelease(t, dirB) // same Message-IDs as A

	_, err := NewServer(ServerConfig{
		ReleaseDir:    dirA,
		Manifest:      mA,
		ExtraReleases: []ReleaseSource{{Dir: dirB, Manifest: mB}},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate message-id") {
		t.Fatalf("NewServer err = %v, want duplicate message-id error", err)
	}

	_, err = NewServer(ServerConfig{
		ReleaseDir:    dirA,
		Manifest:      mA,
		ExtraReleases: []ReleaseSource{{Dir: "", Manifest: nil}},
	})
	if err == nil {
		t.Fatal("NewServer accepted an empty extra release")
	}
}
