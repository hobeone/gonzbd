package app_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

type fakeDownloader struct {
	mu            sync.Mutex
	started       bool
	speedLimit    int64
	speed         float64
	paused        bool
	completions   chan *downloader.ArticleResult
	onJobHopeless func(jobID string)
}

func newFakeDownloader() *fakeDownloader {
	return &fakeDownloader{
		completions: make(chan *downloader.ArticleResult, 10),
	}
}

func (f *fakeDownloader) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *fakeDownloader) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = false
	return nil
}

func (f *fakeDownloader) Completions() <-chan *downloader.ArticleResult {
	return f.completions
}

func (f *fakeDownloader) SetSpeedLimit(bytesPerSec int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.speedLimit = bytesPerSec
}

func (f *fakeDownloader) SetDispatchOptions(maxArtTries, maxArtOpt int, topOnly bool, propagationDelay time.Duration) {
}

func (f *fakeDownloader) UnblockServer(name string) bool {
	return true
}

func (f *fakeDownloader) ServerStatus() []downloader.ServerSnapshot {
	return nil
}

func (f *fakeDownloader) Speed() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speed
}

func (f *fakeDownloader) SpeedLimit() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speedLimit
}

func (f *fakeDownloader) Pause() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
}

func (f *fakeDownloader) Resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = false
}

func (f *fakeDownloader) SetOnJobHopeless(cb func(jobID string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onJobHopeless = cb
}

func (f *fakeDownloader) DisconnectAll() {
}

func TestApplication_FakeDownloaderFlow(t *testing.T) {
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)

	db, err := history.Open(t.Context(), filepath.Join(admin, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)

	fd := newFakeDownloader()
	application, err := app.New(cfg, repo, app.WithDownloader(fd))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	// Create and add a job to the queue
	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.bin",
			Bytes:   100,
			Articles: []nzb.Article{{
				Bytes:  100,
				ID:     "msg1@example.com",
				Number: 1,
			}},
		}},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "test-job", PP: 3}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// Start the application
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_ = application.Queue().SetStatus(job.ID, constants.StatusDownloading)

	// Push article completion result
	fd.completions <- &downloader.ArticleResult{
		JobID:     job.ID,
		MessageID: "msg1@example.com",
		FileIdx:   0,
		Subject:   "test.bin",
		Data:      []byte("hello world"),
		Offset:    0,
	}

	// Wait for the job to complete and get removed/finalized or finish download
	// Since we mock, we can check queue status or history status.
	var entry *history.Entry
	for start := time.Now(); time.Since(start) < 5*time.Second; {
		entry, _ = application.GetHistory(t.Context(), job.ID)
		if entry != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if entry == nil {
		snap := application.Queue().SnapshotJob(job.ID)
		if snap != nil {
			t.Logf("Job in queue: Status=%v, PostProc=%v, IsComplete=%v, Files=%+v",
				snap.Status, snap.PostProc, snap.IsComplete(), snap.Files)
		} else {
			t.Log("Job not found in queue either!")
		}
		t.Fatal("timed out waiting for job to complete in history")
	}

	if entry.Status != string(constants.StatusCompleted) {
		t.Errorf("expected job status StatusCompleted, got %v", entry.Status)
	}
}
