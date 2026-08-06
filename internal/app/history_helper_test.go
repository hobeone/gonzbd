package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// buildHistoryTestJob builds a real *queue.Job with nArticles articles of
// 100 bytes each (TotalBytes = nArticles*100), adds it to a fresh queue, and
// overrides the plain header fields (ID/Name/Filename/Category/Added) —
// still exported, unaffected by the Manifest/Progress split.
func buildHistoryTestJob(t *testing.T, id, name string, added time.Time, nArticles int) (*queue.Queue, *queue.Job) {
	t.Helper()
	parsed := &nzb.NZB{}
	for i := range nArticles {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  fmt.Sprintf("f%d.bin", i),
			Bytes:    100,
			Articles: []nzb.Article{{ID: fmt.Sprintf("a%d@t", i), Bytes: 100, Number: 1}},
		})
	}
	qjob, err := queue.NewJob(parsed, queue.AddOptions{Filename: name + ".nzb", Name: name}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	qjob.ID = id
	qjob.Added = added
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return q, qjob
}

func TestBuildHistoryEntry_Comprehensive(t *testing.T) {
	now := time.Now()

	// 1. Success case, no repair needed, download time > 0.
	// 10 articles x 100 bytes = 1000 total. Fail 1 (100 bytes), complete 8
	// (800 bytes), leave 1 pending — RemainingBytes ends at 100.
	q1, job1q := buildHistoryTestJob(t, "jobid1", "test-job-1", now.Add(-2*time.Hour), 10)
	job1q.Category = "movies"
	if err := q1.MarkJobStarted(job1q.ID, now.Add(-10*time.Second)); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if _, err := q1.MarkArticlesFailed(job1q.ID, []string{"a0@t"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	doneIDs := make([]string, 8)
	for i := range doneIDs {
		doneIDs[i] = fmt.Sprintf("a%d@t", i+1)
	}
	if err := q1.MarkArticlesDone(job1q.ID, doneIDs); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if err := q1.RecordDownload(job1q.ID, "serverA", 1024*1024*5); err != nil { // 5 MB
		t.Fatalf("RecordDownload: %v", err)
	}
	if err := q1.RecordDownload(job1q.ID, "serverB", 1024*1024*15); err != nil { // 15 MB
		t.Fatalf("RecordDownload: %v", err)
	}
	job1q.MarkDownloadFinished(now.Add(-2 * time.Second)) // 8 seconds after start

	job1 := &postproc.Job{
		Queue:       job1q,
		FinalDir:    "/downloads/complete/test-job-1",
		DownloadDir: "/downloads/incomplete/test-job-1",
		StageLog: []postproc.StageLogEntry{
			{
				Stage:   "noop",
				Started: now.Add(-2 * time.Second),
				Elapsed: 1 * time.Second,
			},
		},
	}

	entry1 := buildHistoryEntry(job1)
	if entry1.Status != "Completed" {
		t.Errorf("Expected status Completed, got %q", entry1.Status)
	}
	if entry1.DownloadTime != 8 {
		t.Errorf("Expected download time 8, got %d", entry1.DownloadTime)
	}
	if entry1.PostprocTime != 1 {
		t.Errorf("Expected postproc time 1, got %d", entry1.PostprocTime)
	}
	// Downloaded calculation: TotalBytes(1000) - FailedBytes(100) - RemainingBytes(100) = 800
	if entry1.Downloaded != 800 {
		t.Errorf("Expected downloaded bytes 800, got %d", entry1.Downloaded)
	}
	// Meta: serverA=5.0 MB, serverB=15.0 MB
	expectedMeta := "serverA=5.0 MB, serverB=15.0 MB"
	if entry1.Meta != expectedMeta {
		t.Errorf("Expected Meta %q, got %q", expectedMeta, entry1.Meta)
	}
	// URLInfo default (no repair stage)
	if entry1.URLInfo != "No repair needed" {
		t.Errorf("Expected URLInfo %q, got %q", "No repair needed", entry1.URLInfo)
	}

	// 2. Download duration = 0 case -> defaults to 1
	_, job2q := buildHistoryTestJob(t, "jobid2", "test-job-2", now, 1)
	job2 := &postproc.Job{Queue: job2q}
	entry2 := buildHistoryEntry(job2)
	if entry2.DownloadTime != 1 {
		t.Errorf("Expected download time 1, got %d", entry2.DownloadTime)
	}

	// 3. Failed job, repair stage present, repair success with lines
	_, job3q := buildHistoryTestJob(t, "jobid3", "test-job-3", now, 1)
	job3 := &postproc.Job{
		Queue:    job3q,
		FailMsg:  "some failure message",
		ParError: true,
		StageLog: []postproc.StageLogEntry{
			{
				Stage: "repair",
				Lines: []string{"Repair line 1", "Repair line 2"},
			},
		},
	}
	entry3 := buildHistoryEntry(job3)
	if entry3.Status != "Failed" {
		t.Errorf("Expected status Failed, got %q", entry3.Status)
	}
	if entry3.FailMessage != "some failure message" {
		t.Errorf("Expected FailMessage %q, got %q", "some failure message", entry3.FailMessage)
	}
	if entry3.URLInfo != "Repair line 1" {
		t.Errorf("Expected URLInfo %q, got %q", "Repair line 1", entry3.URLInfo)
	}

	// 4. Repair stage failed with error
	_, job4q := buildHistoryTestJob(t, "jobid4", "test-job-4", now, 1)
	job4 := &postproc.Job{
		Queue: job4q,
		StageLog: []postproc.StageLogEntry{
			{
				Stage: "repair",
				Err:   errors.New("repair tool crashed"),
			},
		},
	}
	entry4 := buildHistoryEntry(job4)
	if entry4.URLInfo != "Repair failed: repair tool crashed" {
		t.Errorf("Expected URLInfo %q, got %q", "Repair failed: repair tool crashed", entry4.URLInfo)
	}

	// 5. Repair stage succeeded, but no lines
	_, job5q := buildHistoryTestJob(t, "jobid5", "test-job-5", now, 1)
	job5 := &postproc.Job{
		Queue: job5q,
		StageLog: []postproc.StageLogEntry{
			{
				Stage: "repair",
				Err:   nil,
				Lines: []string{},
			},
		},
	}
	entry5 := buildHistoryEntry(job5)
	if entry5.URLInfo != "Repair OK" {
		t.Errorf("Expected URLInfo %q, got %q", "Repair OK", entry5.URLInfo)
	}
}

// buildHistoryEntry reports the job's size, and takes it from the promoted
// TotalBytes scalar rather than reaching through the manifest. The entry it
// produces is what the history UI renders and what duplicate detection later
// matches against, so the value must be the same whether or not the manifest
// is still resident — this is the only place a divergence between the scalar
// and the manifest would surface as a wrong number the user keeps.
func TestBuildHistoryEntry_SizeSurvivesEviction(t *testing.T) {
	_, qjob := buildHistoryTestJob(t, "hist-evict", "evicted", time.Now(), 4)
	pj := &postproc.Job{Queue: qjob}

	resident := buildHistoryEntry(pj)
	if resident.Bytes != 400 {
		t.Fatalf("fixture guard: Bytes = %d while resident, want 400", resident.Bytes)
	}

	// A store is required for the queue to evict on pause.
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	q := queue.New(queue.WithStore(queue.NewSQLiteStore(repo.DB(), dir, repo)), queue.WithStateDir(dir))
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Pause(qjob.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := qjob.Manifest(); !errors.Is(err, queue.ErrJobNotResident) {
		t.Fatalf("fixture guard: want ErrJobNotResident after Pause, got %v — nothing is being tested", err)
	}

	evicted := buildHistoryEntry(pj)
	if evicted.Bytes != resident.Bytes {
		t.Errorf("Bytes = %d after eviction, want %d — the recorded size must not depend on manifest residency",
			evicted.Bytes, resident.Bytes)
	}
}

// TestBuildHistoryEntry_DownloadedExcludesDeferredPar2 pins the
// deferred-par2 fix: a job finalized while an on-demand-par2 recovery
// volume is still (deferred, undispatched) in its manifest must record both
// Bytes and Downloaded as only the content actually fetched, not the
// deferred volume's bytes too — the two fields have to describe the same
// file set, since the UI derives a "failed" figure as Bytes-Downloaded.
//
// Not built through buildHistoryTestJob: that helper's queue.AddOptions has
// no OnDemandPar2, and OnDemandPar2 is what makes NewJob mark a recovery
// volume Deferred in the first place (see Job.NewJob's Deferred: isRecovery
// && opts.OnDemandPar2). The fixture is constructed directly here instead of
// widening the shared helper for one test.
func TestBuildHistoryEntry_DownloadedExcludesDeferredPar2(t *testing.T) {
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "content.bin",
				Bytes:    400,
				Articles: []nzb.Article{{ID: "c1@t", Bytes: 400, Number: 1}},
			},
			{
				// Recovery-volume subject: isRecoveryVolume's
				// \.vol\d+[-+]\d+\.par2 pattern, so NewJob marks it
				// IsPar2Recovery and, with OnDemandPar2 below, Deferred.
				Subject:  "content.vol000+01.par2",
				Bytes:    1000,
				Articles: []nzb.Article{{ID: "v1@t", Bytes: 1000, Number: 1}},
			},
		},
	}
	qjob, err := queue.NewJob(parsed, queue.AddOptions{
		Filename:     "deferred.nzb",
		Name:         "deferred",
		OnDemandPar2: true,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	qjob.ID = "hist-deferred-par2"

	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !qjob.HasDeferredPar2() {
		t.Fatal("fixture guard: recovery volume not deferred — NewJob's OnDemandPar2 wiring didn't take, nothing is being tested")
	}
	if err := q.MarkArticlesDone(qjob.ID, []string{"c1@t"}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}

	job := &postproc.Job{Queue: qjob}
	entry := buildHistoryEntry(job)

	// Only the content file's 400 bytes were ever fetched; the deferred
	// 1000-byte recovery volume was never dispatched. Under the old
	// pairing (Bytes = job.Queue.TotalBytes(), the whole-manifest total of
	// 1400, while Downloaded already moved to the expectation) this would
	// have reported Bytes = 1400, Downloaded = 400 — and
	// HistoryRow.svelte's "X of Y received (Y-X failed)" line would have
	// rendered "400 of 1400 (1000 failed)" for a job in which nothing
	// failed. Bytes must describe the same file set as Downloaded.
	if got, want := entry.Bytes, int64(400); got != want {
		t.Errorf("Bytes = %d, want %d (deferred recovery volume must not count toward the advertised size)", got, want)
	}
	if got, want := entry.Downloaded, int64(400); got != want {
		t.Errorf("Downloaded = %d, want %d (deferred recovery volume must not count as downloaded)", got, want)
	}
	if got, want := entry.Bytes-entry.Downloaded, int64(0); got != want {
		t.Errorf("Bytes-Downloaded (the UI's \"failed\" figure) = %d, want %d — nothing failed", got, want)
	}
	if got, want := entry.Completeness, int64(100); got != want {
		t.Errorf("Completeness = %d, want %d", got, want)
	}
}
