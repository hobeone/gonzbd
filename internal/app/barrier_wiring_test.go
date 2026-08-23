package app

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// overlapDetector observes whether two barriers for one job are ever inside
// the store's read-modify-write at the same time.
//
// RunStore.Commit is that window in its entirety now: it reads the file's
// bracketing rows, subtracts the redelivered articles, merges what is left,
// then deletes and re-inserts. The whole of it runs in one transaction, which
// is why the hazard the SERIALISATION exists for is upstream of it — Drain is
// destructive, so two overlapping barriers split one file's articles between
// them and each acks only its own half.
//
// The wrapper does not merely watch — it makes the overlap deterministic. The
// first barrier to reach Commit is held on entry until a second arrives, so if
// concurrency is possible it happens on every run rather than on a lucky
// scheduling. The wait has a deadline, because with the serialisation in place
// no second barrier can arrive and the test must proceed rather than hang.
//
// The alternative — asserting only on the final stored runs — was tried and
// rejected even when the store still replaced rather than merged: which of two
// unserialised barriers commits last was a coin flip, so half the runs would
// report the invariant holding while it did not.
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

func (g *overlapDetector) Commit(ctx context.Context, jobID string, arts []durability.DurableArticle) error {
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
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	err := g.RunStore.Commit(ctx, jobID, arts)

	g.mu.Lock()
	g.open--
	g.mu.Unlock()
	return err
}

func (g *overlapDetector) sawOverlap() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.overlapped
}

// TestCheckpointJob_IsSerialisedPerJob pins the obligation Barrier.Run itself
// cannot discharge.
//
// Run holds no lock — it does I/O from start to finish, and this project bans
// I/O under a lock — so it documents that the caller owns the guarantee of at
// most one barrier in flight per job. This is that guarantee.
//
// Drain is destructive, so two overlapping barriers over one file split its
// articles between them: one gets what the writer was holding and the other
// gets none. Each then acks only its own half while both believe they
// checkpointed the file, and the reports the loser never saw are released by
// whichever cycle confirms — so the articles are neither acked nor re-reported,
// and only a restart recovers them. That is the ground L3 says a restart must
// not lose, arriving through the concurrency door rather than the crash one.
//
// The assertion is on the observed overlap rather than on the lock, because a
// test that checked "the mutex was taken" would pass against any lock, held
// anywhere, for any duration.
func TestCheckpointJob_IsSerialisedPerJob(t *testing.T) {
	application, repo, _ := newLifecycleTestApp(t)
	ctx := t.Context()

	if err := application.assembler.Start(ctx); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.assembler.Stop() })

	// One file, two articles, so a single Drain returns both and a losing
	// commit erases both bits at once.
	const nArts = 2
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: "serialised.bin",
		Bytes:   200,
		Articles: []nzb.Article{
			{ID: "a0@t", Bytes: 100, Number: 1},
			{ID: "a1@t", Bytes: 100, Number: 2},
		},
	}}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "serialised"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.queue.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := application.pipeline.registerFile(job.ID, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	for i := range nArts {
		ref, req := assemblerWrite(job.ID, 0, i, int64(i)*100)
		if err := application.assembler.WriteArticle(ctx, ref, req); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	inner := durability.NewSQLiteRunStore(repo.DB())
	detector := newOverlapDetector(inner)
	application.barrier = durability.NewBarrier(
		detector, application.queue, application, slog.New(slog.DiscardHandler))

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { application.checkpointJob(ctx, job.ID) })
	}
	wg.Wait()

	if got := application.BarrierRuns(); got != 2 {
		t.Fatalf("%d barriers ran, want 2 — the fixture never created the race it is about", got)
	}
	if detector.sawOverlap() {
		t.Error("two barriers for one job were inside the store's read-modify-write at " +
			"the same time; their destructive Drains split the file's articles and each " +
			"acks only its own half")
	}
	runs, err := inner.ForFile(ctx, job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("recorded %d runs, want 1 — the two articles abut in both offset and "+
			"index, so they must merge: %+v", len(runs), runs)
	}
	if got := int(runs[0].LastArtIdx-runs[0].FirstArtIdx) + 1; got != nArts {
		t.Errorf("the recorded run covers %d articles, want %d — one barrier's articles "+
			"were lost to the other's drain, so they are neither acked nor re-reported",
			got, nArts)
	}
}

// assemblerWrite builds the identity and the write request for global article
// globalArt of file fileIdx, at the given file-local offset.
//
// The offset is a parameter rather than derived from globalArt because the two
// index different things: article indices are global to the job, offsets are
// local to the file. An earlier version derived the offset from the article
// index and hardcoded FileIdx 0, which meant a caller asking for file 1 got a
// write to file 0 and a fixture writing file 1's first article had to be built
// by hand to avoid file 0's offsets.
func assemblerWrite(jobID string, fileIdx, globalArt int, offset int64) (assembler.ArticleRef, assembler.WriteRequest) {
	return assembler.ArticleRef{
			JobID: jobID, FileIdx: fileIdx, ArtIdx: int32(globalArt), //nolint:gosec // G115: test article counts are tiny
			MessageID: string(rune('a'+globalArt)) + "@t",
		}, assembler.WriteRequest{
			Offset: offset,
			Data:   make([]byte, 100),
		}
}
