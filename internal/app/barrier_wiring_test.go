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
// their read-modify-write at the same time.
//
// buildExtent's Load and ExtentStore.Commit are the two ends of that window:
// Load reads the stored FileExtent, the barrier adds its drained articles to
// it, and Commit writes the result back. Two barriers overlapping there both
// read the same prior value and the later Commit replaces the earlier one, so
// the committed cache describes neither run.
//
// The wrapper does not merely watch — it makes the overlap deterministic. The
// first barrier to reach LoadFile is held there until a second arrives, so if
// concurrency is possible it happens on every run rather than on a lucky
// scheduling. The wait has a deadline, because with the serialisation in place
// no second barrier can arrive and the test must proceed rather than hang.
//
// LoadFile and not Load: Barrier.priorExtent reads one file by primary key.
// It filtered a whole-job Load until that turned out to be quadratic in a
// job's file count, and this wrapper hooked Load. Hooking a method the barrier
// no longer calls costs nothing visible — the embedded store still answers, so
// the test stays green while the counter it asserts on never moves.
//
// The alternative — asserting only on the final committed bitmap — was tried
// and rejected: which of two unserialised barriers commits last is a coin
// flip, so half the runs would report the invariant holding while it did not.
type overlapDetector struct {
	durability.ExtentStore

	mu         sync.Mutex
	open       int
	overlapped bool

	secondOnce sync.Once
	second     chan struct{}
}

func newOverlapDetector(inner durability.ExtentStore) *overlapDetector {
	return &overlapDetector{ExtentStore: inner, second: make(chan struct{})}
}

func (g *overlapDetector) LoadFile(ctx context.Context, jobID string, fileIdx int32) (durability.FileExtent, bool, error) {
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
	return g.ExtentStore.LoadFile(ctx, jobID, fileIdx)
}

func (g *overlapDetector) Commit(ctx context.Context, jobID string, exts []durability.FileExtent) error {
	g.mu.Lock()
	g.open--
	g.mu.Unlock()
	return g.ExtentStore.Commit(ctx, jobID, exts)
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
// Two overlapping barriers over one job interleave phase 3: both load the same
// stored FileExtent, each adds only the articles its own Drain returned, and
// the second Commit replaces the first. Drain is destructive, so one of the
// two gets the articles and the other gets none — and the loser's commit
// silently erases the winner's durable bits. The job then re-downloads bytes
// that are on disk, which is the ground L3 says a restart must not lose.
//
// The assertion is on the committed bitmap rather than on the lock, because a
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
		if err := application.assembler.WriteArticle(ctx, assemblerWrite(job.ID, i)); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	inner := durability.NewSQLiteExtentStore(repo.DB())
	detector := newOverlapDetector(inner)
	application.barrier = durability.NewBarrier(
		durability.NewSQLiteFactLog(repo.DB()), detector,
		application.queue, application, slog.New(slog.DiscardHandler))

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { application.checkpointJob(ctx, job.ID) })
	}
	wg.Wait()

	if got := application.BarrierRuns(); got != 2 {
		t.Fatalf("%d barriers ran, want 2 — the fixture never created the race it is about", got)
	}
	if detector.sawOverlap() {
		t.Error("two barriers for one job were inside their read-modify-write at the same " +
			"time; whichever commits last erases the other's durable bits and the job " +
			"re-downloads bytes that are already on disk")
	}
	exts, err := inner.Load(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) != 1 {
		t.Fatalf("loaded %d extents, want 1", len(exts))
	}
	if got := exts[0].Durable.Count(); got != nArts {
		t.Errorf("committed durable bits = %d, want %d — two barriers over one job "+
			"interleaved their read-modify-write and the later commit erased the earlier "+
			"one's bits, so the job re-downloads bytes that are already on disk",
			got, nArts)
	}
}

// assemblerWrite builds the write request for article i of file 0, at the
// offset the fixture's 100-byte articles imply.
func assemblerWrite(jobID string, i int) assembler.WriteRequest {
	return assembler.WriteRequest{
		JobID: jobID, FileIdx: 0, ArtIdx: int32(i), //nolint:gosec // G115: test article counts are tiny
		MessageID: string(rune('a'+i)) + "@t",
		Offset:    int64(i) * 100,
		Data:      make([]byte, 100),
	}
}
