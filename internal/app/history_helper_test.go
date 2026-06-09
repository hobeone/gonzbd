package app

import (
	"errors"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

func TestBuildHistoryEntry_Comprehensive(t *testing.T) {
	now := time.Now()

	// 1. Success case, no repair needed, download time > 0
	job1 := &postproc.Job{
		Queue: &queue.Job{
			ID:               "jobid1",
			Name:             "test-job-1",
			Filename:         "test-job-1.nzb",
			Category:         "movies",
			Added:            now.Add(-2 * time.Hour),
			DownloadStarted:  now.Add(-10 * time.Second),
			DownloadFinished: now.Add(-2 * time.Second), // 8 seconds
			TotalBytes:       1000,
			FailedBytes:      100,
			RemainingBytes:   50,
			ServerStats: map[string]int64{
				"serverA": 1024 * 1024 * 5,  // 5 MB
				"serverB": 1024 * 1024 * 15, // 15 MB
			},
		},
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
	// Downloaded calculation: TotalBytes(1000) - FailedBytes(100) - RemainingBytes(50) = 850
	if entry1.Downloaded != 850 {
		t.Errorf("Expected downloaded bytes 850, got %d", entry1.Downloaded)
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
	job2 := &postproc.Job{
		Queue: &queue.Job{
			DownloadStarted:  now,
			DownloadFinished: now, // 0 seconds
		},
	}
	entry2 := buildHistoryEntry(job2)
	if entry2.DownloadTime != 1 {
		t.Errorf("Expected download time 1, got %d", entry2.DownloadTime)
	}

	// 3. Failed job, repair stage present, repair success with lines
	job3 := &postproc.Job{
		Queue: &queue.Job{
			Name: "test-job-3",
		},
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
	job4 := &postproc.Job{
		Queue: &queue.Job{},
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
	job5 := &postproc.Job{
		Queue: &queue.Job{},
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
