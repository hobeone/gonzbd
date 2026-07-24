package app

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
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
