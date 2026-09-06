package app

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

type fakeDownloader struct {
	mu sync.Mutex
	// startErr, when non-nil, is returned by Start instead of starting.
	// Lets tests drive Application.Start's failure paths.
	startErr         error
	started          bool
	speedLimit       int64
	speed            float64
	paused           bool
	completions      chan *downloader.ArticleResult
	maxArtTries      int
	maxArtOpt        int
	topOnly          bool
	propagationDelay time.Duration
	serverStatus     []downloader.ServerSnapshot
}

func newFakeDownloader() *fakeDownloader {
	return &fakeDownloader{
		completions: make(chan *downloader.ArticleResult, 10),
	}
}

func (f *fakeDownloader) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maxArtTries = maxArtTries
	f.maxArtOpt = maxArtOpt
	f.topOnly = topOnly
	f.propagationDelay = propagationDelay
}

func (f *fakeDownloader) UnblockServer(name string) bool {
	return true
}

func (f *fakeDownloader) ServerStatus() []downloader.ServerSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.serverStatus
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
	application, err := New(cfg, repo, WithDownloader(fd))
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
	j, hdr, err := BuildIngestJob(application.config, parsed, "test.nzb", types.FetchOptions{NzbName: "test-job", PP: 3}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.AddJob(t.Context(), j, hdr, nil, false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Start the application
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Cancelling ctx alone does not stop the assembler worker: Shutdown drains
	// it explicitly (see Application.Shutdown's ordering). Without this the
	// worker outlives the test and goleak fails the package.
	t.Cleanup(func() {
		if err := application.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	// Push article completion result
	fd.completions <- &downloader.ArticleResult{
		JobID:     j.ID(),
		MessageID: "msg1@example.com",
		FileIdx:   0,
		Subject:   "test.bin",
		Data:      []byte("hello world"),
		Offset:    0,
	}

	// Wait for post-processing to complete via the deterministic channel
	// signal, not polling. Project anti-patterns forbid time.Sleep for
	// synchronization.
	select {
	case <-application.PostProcComplete():
	case <-time.After(5 * time.Second):
		if row, ok := application.Dispatcher().Row(j.ID()); ok {
			t.Logf("Job in dispatcher: Status=%v", row.Status())
		}
		t.Fatal("timed out waiting for PostProcComplete")
	}

	// PostProcComplete fired — the job should now be in history.
	entry, err := application.GetHistory(t.Context(), j.ID())
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if entry == nil {
		t.Fatal("expected job in history after PostProcComplete, got nil")
	}

	if entry.Status != string(constants.StatusCompleted) {
		t.Errorf("expected job status StatusCompleted, got %v", entry.Status)
	}
}
