package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

type blockingEmitter struct {
	block   chan struct{}
	enabled atomic.Bool
}

func (b *blockingEmitter) Broadcast(e Event) {
	if b.enabled.Load() && e.Type == "queue_updated" && b.block != nil {
		<-b.block
	}
}

// TestShutdown_SaturatedCompletionChannel_NoDroppedCompletions tests that when
// internalFileComplete channel is saturated (>128 completions), fallback goroutines
// are tracked by app.wg and do not discard their completion events on ctx.Done() during Shutdown.
func TestShutdown_SaturatedCompletionChannel_NoDroppedCompletions(t *testing.T) {
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
	blockCh := make(chan struct{})
	emitter := &blockingEmitter{block: blockCh}
	application, err := New(cfg, repo, WithDownloader(fd), WithEventEmitter(emitter))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	const numFiles = 150
	files := make([]nzb.File, 0, numFiles)
	for i := range numFiles {
		files = append(files, nzb.File{
			Subject: fmt.Sprintf("file-%03d.bin", i),
			Bytes:   100,
			Articles: []nzb.Article{{
				Bytes:  100,
				ID:     fmt.Sprintf("art-%03d@example.com", i),
				Number: 1,
			}},
		})
	}
	parsed := &nzb.NZB{Files: files}
	j, hdr, err := BuildIngestJob(application.config, parsed, "test-sat-job.nzb", types.FetchOptions{NzbName: "test-sat-job"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.AddJob(t.Context(), j, hdr, nil, false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	emitter.enabled.Store(true)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Send 150 completions. Event 0 blocks watchCompletions inside emitter.Broadcast.
	// Events 1-128 fill internalFileComplete (capacity 128).
	// Events 129-149 take the fallback goroutine path.
	for i := range numFiles {
		application.TriggerOnFileComplete(j.ID(), i)
	}

	// When app.cancel() runs during Shutdown, unblock emitter so watchCompletions
	// and drainCompletions can process all completion events.
	go func() {
		<-application.Context().Done()
		// Sleep briefly so untracked goroutines on unpatched main select <-ctx.Done().
		time.Sleep(50 * time.Millisecond)
		close(blockCh)
	}()

	if err := application.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Verify all 150 files were marked complete.
	dj, ok := application.Dispatcher().Job(j.ID())
	if !ok || dj == nil {
		t.Fatalf("job %s not found in dispatcher", j.ID())
	}
	p := dj.Progress()
	if p == nil {
		t.Fatal("job progress is nil")
	}
	for fi := range numFiles {
		if !p.FileComplete(fi) {
			t.Errorf("file %d Complete = false, want true", fi)
		}
	}
	if !dj.IsComplete() {
		t.Error("job.IsComplete() = false, want true")
	}
}
