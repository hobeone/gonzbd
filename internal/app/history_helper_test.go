package app

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// buildHistoryTestJob builds a real *job.Job with nArticles articles of
// 100 bytes each (TotalBytes = nArticles*100), adds it to a fresh dispatcher, and
// overrides the plain header fields (ID/Name/Filename/Category/Added).
func buildHistoryTestJob(t *testing.T, id, name string, added time.Time, nArticles int) (*dispatch.Dispatcher, *job.Job) {
	t.Helper()
	parsed := &nzb.NZB{}
	for i := range nArticles {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  fmt.Sprintf("f%d.bin", i),
			Bytes:    100,
			Articles: []nzb.Article{{ID: fmt.Sprintf("a%d@t", i), Bytes: 100, Number: 1}},
		})
	}
	app := newTestApplication(t)
	j, hdr, err := BuildIngestJob(app.config, parsed, name+".nzb", types.FetchOptions{NzbName: name, JobID: id}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	j.SetAdded(added)
	if err := app.Dispatcher().Add(j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return app.Dispatcher(), j
}

func TestBuildHistoryEntry_Comprehensive(t *testing.T) {
	t.Parallel()
	now := time.Now()

	// 1. Success case, no repair needed, download time > 0.
	// 10 articles x 100 bytes = 1000 total. Fail 1 (100 bytes), complete 8
	// (800 bytes), leave 1 pending — RemainingBytes ends at 100.
	disp1, job1q := buildHistoryTestJob(t, "jobid1", "test-job-1", now.Add(-2*time.Hour), 10)
	if err := job1q.MarkJobStarted(now.Add(-10 * time.Second)); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	ackFailed(t, disp1, job1q.ID(), "a0@t")
	doneIDs := make([]string, 8)
	for i := range doneIDs {
		doneIDs[i] = fmt.Sprintf("a%d@t", i+1)
	}
	ackDone(t, disp1, job1q.ID(), doneIDs...)
	if err := job1q.RecordDownload("serverA", 1024*1024*5); err != nil { // 5 MB
		t.Fatalf("RecordDownload: %v", err)
	}
	if err := job1q.RecordDownload("serverB", 1024*1024*15); err != nil { // 15 MB
		t.Fatalf("RecordDownload: %v", err)
	}
	// 8 seconds after start.
	if err := job1q.MarkDownloadFinished(now.Add(-2 * time.Second)); err != nil {
		t.Fatalf("MarkDownloadFinished: %v", err)
	}

	job1 := &postproc.Job{
		Job:         job1q,
		Category:    "movies",
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
	job2 := &postproc.Job{Job: job2q}
	entry2 := buildHistoryEntry(job2)
	if entry2.DownloadTime != 1 {
		t.Errorf("Expected download time 1, got %d", entry2.DownloadTime)
	}

	// 3. Failed job, repair stage present, repair success with lines
	_, job3q := buildHistoryTestJob(t, "jobid3", "test-job-3", now, 1)
	job3 := &postproc.Job{
		Job:      job3q,
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
		Job: job4q,
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
		Job: job5q,
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
	t.Parallel()
	_, qjob := buildHistoryTestJob(t, "hist-evict", "evicted", time.Now(), 4)
	pj := &postproc.Job{Job: qjob}

	resident := buildHistoryEntry(pj)
	if resident.Bytes != 400 {
		t.Fatalf("fixture guard: Bytes = %d while resident, want 400", resident.Bytes)
	}

	qjob.Evict()
	if _, err := qjob.Manifest(); !errors.Is(err, job.ErrNotResident) {
		t.Fatalf("fixture guard: want ErrNotResident after Evict, got %v", err)
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
func TestBuildHistoryEntry_DownloadedExcludesDeferredPar2(t *testing.T) {
	t.Parallel()
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "content.bin",
				Bytes:    400,
				Articles: []nzb.Article{{ID: "c1@t", Bytes: 400, Number: 1}},
			},
			{
				Subject:  "content.vol000+01.par2",
				Bytes:    1000,
				Articles: []nzb.Article{{ID: "v1@t", Bytes: 1000, Number: 1}},
			},
		},
	}
	app := newTestApplication(t)
	app.config.With(func(c *config.Config) {
		c.Downloads.OnDemandPar2 = true
	})
	qjob, hdr, err := BuildIngestJob(app.config, parsed, "deferred.nzb", types.FetchOptions{
		NzbName: "deferred",
		JobID:   "hist-deferred-par2",
	}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}

	if err := app.Dispatcher().Add(qjob, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !qjob.HasDeferredPar2() {
		t.Fatal("fixture guard: recovery volume not deferred — OnDemandPar2 wiring didn't take, nothing is being tested")
	}
	ackDone(t, app.Dispatcher(), qjob.ID(), "c1@t")

	ppJob := &postproc.Job{Job: qjob}
	entry := buildHistoryEntry(ppJob)

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
