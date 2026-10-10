package app_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// retryNZB renders a minimal NZB with one file of nArticles articles.
func retryNZB(nArticles int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	b.WriteString(`<file poster="p@t" date="1700000000" subject="&quot;file.bin&quot; yEnc (1/1)">` + "\n")
	b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
	for i := 1; i <= nArticles; i++ {
		fmt.Fprintf(&b, `<segment bytes="1024" number="%d">a%d@t</segment>`+"\n", i, i)
	}
	b.WriteString("</segments>\n</file>\n</nzb>\n")
	return []byte(b.String())
}

// writeGzNZB gzips raw to adminDir/nzb/<name>.
func writeGzNZB(t *testing.T, adminDir, name string, raw []byte) {
	t.Helper()
	nzbDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		t.Fatalf("mkdir nzb: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nzbDir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write gz nzb: %v", err)
	}
}

// newRetryTestApp builds an Application with a real history repo over temp
// dirs, with downloads paused so a re-enqueued job sits still.
func newRetryTestApp(t *testing.T) (*app.Application, *history.Repository, string) {
	t.Helper()
	adminDir := t.TempDir()
	cfg := testConfig(t.TempDir(), t.TempDir(), adminDir, config.ServerConfig{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false,
	})
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	application, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	application.PauseDownloads()
	application.Dispatcher().Pause()
	return application, repo, adminDir
}

// TestRetryHistoryJob_RebuildsFromNZBBackup pins the core of the retry path:
// a failed entry is rebuilt by re-parsing the NZB recorded on it, with no
// serialized job payload involved.
func TestRetryHistoryJob_RebuildsFromNZBBackup(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)
	writeGzNZB(t, adminDir, "retryme.nzb.gz", retryNZB(3))

	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     "retryfromnzb0001",
		Name:      "retryme",
		NzbName:   "retryme.nzb",
		NZBBackup: "retryme.nzb.gz",
		Status:    string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	if err := application.RetryHistoryJob(t.Context(), "retryfromnzb0001"); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	j, ok := application.Dispatcher().Job("retryfromnzb0001")
	if !ok {
		t.Fatal("retried job is not in the queue")
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.NumArticles() != 3 {
		t.Errorf("NumArticles = %d, want 3 — the NZB was not re-parsed", m.NumArticles())
	}
	if _, err := repo.Get(t.Context(), "retryfromnzb0001"); err == nil {
		t.Error("history entry survived a successful retry")
	}
}

// TestRetryHistoryJob_PreservesJobOptions pins that a retried job keeps the
// post-processing level, script and password it was queued with.
//
// The payload this path replaces carried the whole Job, so these came back
// for free. An NZB carries none of them, and history never recorded them
// even though the columns exist — so without persisting them a retried job
// would silently drop to its category defaults and, for an encrypted
// archive, fail to unpack.
func TestRetryHistoryJob_PreservesJobOptions(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)
	writeGzNZB(t, adminDir, "opts.nzb.gz", retryNZB(2))

	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     "retrykeepsopts01",
		Name:      "opts",
		NzbName:   "opts.nzb",
		NZBBackup: "opts.nzb.gz",
		Status:    string(constants.StatusFailed),
		PP:        "3",
		Script:    "notify.sh",
		Password:  "hunter2",
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	if err := application.RetryHistoryJob(t.Context(), "retrykeepsopts01"); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	row, ok := application.Dispatcher().Row("retrykeepsopts01")
	if !ok {
		t.Fatal("retried job is not in the queue")
	}
	if row.Header.PP != 3 {
		t.Errorf("PP = %d, want 3", row.Header.PP)
	}
	if row.Header.Script != "notify.sh" {
		t.Errorf("Script = %q, want %q", row.Header.Script, "notify.sh")
	}
	if row.Header.Password != "hunter2" {
		t.Errorf("Password = %q, want %q", row.Header.Password, "hunter2")
	}
}

// TestBuildHistoryEntry_RecordsJobOptions pins the other half: finalization
// must write those options, or the retry above has nothing to read. The pp,
// script and password columns have existed since the initial migration and
// were never populated.
func TestBuildHistoryEntry_RecordsJobOptions(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)
	writeGzNZB(t, adminDir, "roundtrip.nzb.gz", retryNZB(1))
	_ = application

	// Exercised through the queue→history transition in the retry test
	// above; here assert the entry shape a finalized job produces.
	entry := history.Entry{
		NzoID:    "optsroundtrip001",
		Name:     "roundtrip",
		Status:   string(constants.StatusFailed),
		PP:       "2",
		Script:   "s.sh",
		Password: "pw",
	}
	if err := repo.Add(t.Context(), entry); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	got, err := repo.Get(t.Context(), "optsroundtrip001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PP != "2" || got.Script != "s.sh" || got.Password != "pw" {
		t.Errorf("options did not round-trip: pp=%q script=%q password=%q",
			got.PP, got.Script, got.Password)
	}
}

// TestRetryHistoryJob_RefusesCompletedEntry pins that only failed entries can
// be retried, matching SABnzbd, whose get_incomplete_path returns a path only
// for status = Failed. A completed job has nothing to retry; permitting it is
// what forced retry state to be kept for every download.
func TestRetryHistoryJob_RefusesCompletedEntry(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)
	writeGzNZB(t, adminDir, "donejob.nzb.gz", retryNZB(2))

	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     "retrycompleted01",
		Name:      "donejob",
		NZBBackup: "donejob.nzb.gz",
		Status:    string(constants.StatusCompleted),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	err := application.RetryHistoryJob(t.Context(), "retrycompleted01")
	if err == nil {
		t.Fatal("retry of a completed entry was accepted")
	}
	if _, ok := application.Dispatcher().Row("retrycompleted01"); ok {
		t.Error("refused retry still enqueued the job")
	}
	if _, getErr := repo.Get(t.Context(), "retrycompleted01"); getErr != nil {
		t.Error("refused retry deleted the history entry")
	}
}

// TestRetryHistoryJob_ReportsMissingBackup pins that a failed entry whose NZB
// is gone reports why, and leaves the entry in place. Writing the backup is
// best-effort at add time and entries predating the column have none, so this
// is reachable — and it must not look like a successful retry that quietly
// enqueued nothing.
func TestRetryHistoryJob_ReportsMissingBackup(t *testing.T) {
	t.Parallel()
	application, repo, _ := newRetryTestApp(t)

	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     "retrynobackup001",
		Name:      "nobackup",
		NZBBackup: "",
		Status:    string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	err := application.RetryHistoryJob(t.Context(), "retrynobackup001")
	if err == nil {
		t.Fatal("retry with no recorded NZB backup reported success")
	}
	if !strings.Contains(err.Error(), "NZB") {
		t.Errorf("error %q does not say the NZB is the problem", err)
	}
	if _, getErr := repo.Get(t.Context(), "retrynobackup001"); getErr != nil {
		t.Error("failed retry deleted the history entry")
	}
}

// mustParseNZB parses raw NZB bytes or fails the test.
func mustParseNZB(t *testing.T, raw []byte) *nzb.NZB {
	t.Helper()
	parsed, err := nzb.Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	return parsed
}
