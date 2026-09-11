package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// These two tests live in package app (not app_test) because they drive
// Evict then Hydrate directly through Application.residency, an unexported
// field — the pattern residency_test.go and residency_hydration_test.go
// already use. Job.Evict nils only the manifest and leaves JobProgress
// intact, so both tests evict a job that has downloaded nothing rather than
// an already-live one: an already-live job already carries the right policy
// in memory, and neutering the fix would change nothing there (#329).

// evictNZBFile appends one <file> element carrying nArticles segments to b.
func evictNZBFile(b *strings.Builder, filename, idPrefix string, nArticles int) {
	fmt.Fprintf(b, `<file poster="p@t" date="1700000000" subject="&quot;%s&quot; yEnc (1/%d)">`+"\n", filename, nArticles)
	b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
	for i := 1; i <= nArticles; i++ {
		fmt.Fprintf(b, `<segment bytes="1024" number="%d">%s%d@t</segment>`+"\n", i, idPrefix, i)
	}
	b.WriteString("</segments>\n</file>\n")
}

// evictNZBWithRecoveryVolume renders a two-file NZB: a payload file plus a
// par2 recovery volume, so BuildIngestJob classifies the second file as a
// deferrable recovery volume.
func evictNZBWithRecoveryVolume(nPayload, nRecovery int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	evictNZBFile(&b, "payload.bin", "p", nPayload)
	evictNZBFile(&b, "payload.vol000+01.par2", "r", nRecovery)
	b.WriteString("</nzb>\n")
	return []byte(b.String())
}

// recoveryFileIndexIn returns the one manifest index FileIsPar2Recovery
// reports true for, failing the test if there is not exactly one.
func recoveryFileIndexIn(t *testing.T, m *job.Manifest) int {
	t.Helper()
	idx := -1
	for i := range m.NumFiles() {
		if m.FileIsPar2Recovery(i) {
			if idx != -1 {
				t.Fatalf("manifest has more than one recovery volume: %d and %d", idx, i)
			}
			idx = i
		}
	}
	if idx == -1 {
		t.Fatal("manifest has no recovery volume — fixture is wrong")
	}
	return idx
}

// writeGzNZBFile gzips raw to adminDir/nzb/<name>.
func writeGzNZBFile(t *testing.T, adminDir, name string, raw []byte) {
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

// seedJobFilesRowIn inserts a job_files row directly, standing in for the row
// a previously FAILED attempt left behind — job_finalizer.go's
// shouldDeleteDurability keeps job_files for a failed job rather than
// deleting it, so this is the state a retry actually finds on disk.
func seedJobFilesRowIn(t *testing.T, db *sql.DB, jobID string, fileIndex int, complete bool, fetch job.FetchPolicy) {
	t.Helper()
	c := 0
	if complete {
		c = 1
	}
	if _, err := db.Exec(
		`INSERT INTO job_files (job_id, file_index, complete, filename, assembled_crc32, fetch_policy)
		 VALUES (?, ?, ?, '', 0, ?)`,
		jobID, fileIndex, c, int(fetch)); err != nil {
		t.Fatalf("seed job_files row: %v", err)
	}
}

// seedHistoryJobFilesRowIn inserts a history_job_files row — the retained
// per-file progress historyFileProgress reads to build RetryHistoryJob's
// overlay. articleCount must match the re-parsed NZB's file range or
// retainedMatchesManifest rejects the whole overlay.
func seedHistoryJobFilesRowIn(t *testing.T, db *sql.DB, jobID string, fileIndex int, complete bool, articleCount int) {
	t.Helper()
	c := 0
	if complete {
		c = 1
	}
	if _, err := db.Exec(
		`INSERT INTO history_job_files (job_id, file_index, complete, filename, assembled_crc32, article_count, fetch_policy)
		 VALUES (?, ?, ?, '', 0, ?, 0)`,
		jobID, fileIndex, c, articleCount); err != nil {
		t.Fatalf("seed history_job_files row: %v", err)
	}
}

// newEvictionTestApp builds an Application with a real history repo over
// temp dirs, downloads paused so a job sits still.
func newEvictionTestApp(t *testing.T) (*Application, *history.Repository, string) {
	t.Helper()
	adminDir := t.TempDir()
	cfg := testConfigInternal(t, adminDir)
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	application, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	application.PauseDownloads()
	application.Dispatcher().Pause()
	return application, repo, adminDir
}

// TestRetryHistoryJob_SurvivesEviction pins that the fetch policy a retry
// derives is what a later eviction and re-hydration reads back too — not
// merely what is briefly true in memory right after RetryHistoryJob returns.
// Task 3b's seed-then-flush is what makes this hold: without it, job_files
// still carries the failed attempt's fetch_policy (FetchAlways, seeded
// below) and Hydrate's restoreJobFiles/RestoreFetchPolicy call reapplies
// that stale value over the re-derived FetchIfNeeded the moment the
// dispatcher evicts and re-hydrates the retried job (#329, case d).
//
// job_files must be seeded with the failed attempt's OWN fetch_policy
// (FetchAlways here), not left empty: seedJobFiles' ON CONFLICT DO NOTHING
// only fills a missing row, so an empty job_files table would let 3a's seed
// (which already writes the correct derived policy) create a correct row on
// its own — passing this test even with Mark/Flush deleted, and reporting
// Task 5's mutation SURVIVED instead of killed.
func TestRetryHistoryJob_SurvivesEviction(t *testing.T) {
	application, repo, adminDir := newEvictionTestApp(t)

	writeGzNZBFile(t, adminDir, "survivesevict.nzb.gz", evictNZBWithRecoveryVolume(2, 1))

	const id = "retrysurviveevict1"
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      "survivesevict",
		NzbName:   "survivesevict.nzb",
		NZBBackup: "survivesevict.nzb.gz",
		Status:    string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	seedHistoryJobFilesRowIn(t, repo.DB(), id, 0, false, 2)
	seedHistoryJobFilesRowIn(t, repo.DB(), id, 1, false, 1)
	// The failed attempt's own job_files row: on-demand par2 was on and the
	// recovery volume was held.
	seedJobFilesRowIn(t, repo.DB(), id, 0, false, job.FetchAlways)
	seedJobFilesRowIn(t, repo.DB(), id, 1, false, job.FetchAlways)

	if err := application.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	j, ok := application.Dispatcher().Job(id)
	if !ok {
		t.Fatal("retried job is not in the queue")
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	idx := recoveryFileIndexIn(t, m)
	if got := j.Progress().FileFetchPolicy(idx); got != job.FetchIfNeeded {
		t.Fatalf("precondition: FileFetchPolicy(%d) = %v right after retry, want FetchIfNeeded", idx, got)
	}

	application.residency.Evict(id)
	if j.Resident() {
		t.Fatal("Evict must leave the job non-resident")
	}
	if err := application.residency.Hydrate(context.Background(), id); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	if got := j.Progress().FileFetchPolicy(idx); got != job.FetchIfNeeded {
		t.Errorf("FileFetchPolicy(%d) = %v after evict+re-hydrate, want FetchIfNeeded — job_files "+
			"still carried the failed attempt's stale policy, so 3b's seed-and-flush did not "+
			"overwrite it before the dispatcher could evict the retried job", idx, got)
	}
}

// TestAddJob_FreshRecoveryVolumeSurvivesEviction pins the ingest-path half of
// the invariant: a fresh on-demand-par2 job evicted and re-hydrated before
// any article completes must keep its recovery volume at FetchIfNeeded.
// checkpointer.Mark only fires on download/ack progress, so a job that has
// downloaded nothing is never flushed — seedJobFiles authoring the derived
// policy at INSERT time (Task 3a) is the only thing standing between this
// and the volume silently downloading anyway (#329, case e).
func TestAddJob_FreshRecoveryVolumeSurvivesEviction(t *testing.T) {
	application, _, _ := newEvictionTestApp(t)
	if !application.config.GetDownloads().OnDemandPar2 {
		t.Fatal("precondition: on-demand par2 must be on for this fixture")
	}

	parsed := &nzb.NZB{Files: []nzb.File{
		{
			Subject: "payload.bin",
			Bytes:   1024,
			Articles: []nzb.Article{
				{ID: "fresh-p1@t", Bytes: 1024, Number: 1},
			},
		},
		{
			Subject: "payload.vol000+01.par2",
			Bytes:   512,
			Articles: []nzb.Article{
				{ID: "fresh-r1@t", Bytes: 512, Number: 1},
			},
		},
	}}

	const id = "freshaddjobevict1"
	j, hdr, err := BuildIngestJob(application.config, parsed, "fresh.nzb", types.FetchOptions{JobID: id}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.AddJob(t.Context(), j, hdr, []byte("raw nzb bytes"), false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	idx := recoveryFileIndexIn(t, m)
	if got := j.Progress().FileFetchPolicy(idx); got != job.FetchIfNeeded {
		t.Fatalf("precondition: FileFetchPolicy(%d) = %v right after AddJob, want FetchIfNeeded", idx, got)
	}

	application.residency.Evict(id)
	if j.Resident() {
		t.Fatal("Evict must leave the job non-resident")
	}
	if err := application.residency.Hydrate(t.Context(), id); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	if got := j.Progress().FileFetchPolicy(idx); got != job.FetchIfNeeded {
		t.Errorf("FileFetchPolicy(%d) = %v after evict+re-hydrate of a job that downloaded "+
			"nothing, want FetchIfNeeded — seedJobFiles wrote the hardcoded FetchAlways default "+
			"instead of the derived policy, and no checkpointer flush had fired yet to correct it", idx, got)
	}
}
