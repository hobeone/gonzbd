package app

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// overlapDetector observes whether two barriers for one job are ever inside
// the store's read-modify-write at the same time.
type overlapDetector struct {
	durability.RunStore

	mu         sync.Mutex
	open       int
	overlapped bool

	secondOnce sync.Once
	second     chan struct{}
}

func newOverlapDetector(inner durability.RunStore) *overlapDetector {
	return &overlapDetector{RunStore: inner, second: make(chan struct{})}
}

func (g *overlapDetector) Commit(ctx context.Context, jobID string, arts []durability.DurableArticle) ([]durability.Collision, error) {
	g.mu.Lock()
	g.open++
	open := g.open
	if open > 1 {
		g.overlapped = true
	}
	g.mu.Unlock()

	if open > 1 {
		g.secondOnce.Do(func() { close(g.second) })
	} else {
		select {
		case <-g.second:
		case <-time.After(30 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	cols, err := g.RunStore.Commit(ctx, jobID, arts)

	g.mu.Lock()
	g.open--
	g.mu.Unlock()
	return cols, err
}

func (g *overlapDetector) sawOverlap() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.overlapped
}

// TestCheckpointJob_IsSerialisedPerJob pins the obligation Barrier.Run itself
// cannot discharge.
func TestCheckpointJob_IsSerialisedPerJob(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	ctx := t.Context()

	if err := application.assembler.Start(ctx); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.assembler.Stop() })

	const nArts = 2
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: "serialised.bin",
		Bytes:   200,
		Articles: []nzb.Article{
			{ID: "a0@t", Bytes: 100, Number: 1},
			{ID: "a1@t", Bytes: 100, Number: 2},
		},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "serialised.nzb", types.FetchOptions{NzbName: "serialised"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := application.pipeline.registerFile(j.ID(), 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	for i := range nArts {
		ref, req := assemblerWrite(j.ID(), 0, i, int64(i)*100)
		if err := application.assembler.WriteArticle(ctx, ref, req); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	inner := durability.NewSQLiteRunStore(repo.DB())
	detector := newOverlapDetector(inner)
	application.barrier = durability.NewBarrier(
		detector, application, application, slog.New(slog.DiscardHandler))

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { application.checkpointJob(ctx, j.ID()) })
	}
	wg.Wait()

	if got := application.BarrierRuns(); got != 2 {
		t.Fatalf("%d barriers ran, want 2", got)
	}
	if detector.sawOverlap() {
		t.Error("two barriers for one job were inside the store's read-modify-write at the same time")
	}
	runs, err := inner.ForFile(ctx, j.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("recorded %d runs, want 1: %+v", len(runs), runs)
	}
	if got := int(runs[0].LastArtIdx-runs[0].FirstArtIdx) + 1; got != nArts {
		t.Errorf("the recorded run covers %d articles, want %d", got, nArts)
	}
}

func assemblerWrite(jobID string, fileIdx, globalArt int, offset int64) (assembler.ArticleRef, assembler.WriteRequest) {
	return assembler.ArticleRef{
		JobID: jobID, FileIdx: fileIdx, ArtIdx: int32(globalArt), //nolint:gosec // G115: test article counts are tiny
		MessageID: string(rune('a'+globalArt)) + "@t",
	}, assembler.WriteRequest{
		Offset: offset,
		Data:   make([]byte, 100),
	}
}
