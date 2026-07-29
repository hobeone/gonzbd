package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// #229 asked whether the two store calls inside PromoteNext
// (RestoreJobProgress:573, Update:596) are worth hoisting out of q.mu. This
// file measures the actual cost instead of guessing: a burst-size sweep
// isolating the SQLite-added cost from the RAM-only critical section, and a
// contention check against a concurrently running ForEachUnfinishedArticle
// scan -- the real concurrent counterpart, since nothing calls PromoteNext on
// a periodic loop the way the downloader's dispatch loop calls
// ForEachUnfinishedArticle every pass.

// buildPromoteCandidate builds one queued job with a manifest already
// attached, matching a resident job's shape so PromoteNext takes the
// cached-manifest path (queue.go's PromoteNext Step 2) instead of a real disk
// read -- isolating the benchmark to the lock/SQLite cost the two
// under-lock calls add, per the manifest's own note about that branch.
func buildPromoteCandidate(tb testing.TB, idx int) *Job {
	tb.Helper()
	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject:  fmt.Sprintf("promote-bench-%d.bin", idx),
			Bytes:    65536,
			Articles: []nzb.Article{{ID: fmt.Sprintf("art-%d@bench.test", idx), Bytes: 65536, Number: 1}},
		}},
	}
	job, err := NewJob(parsed, AddOptions{Filename: fmt.Sprintf("promote-bench-%d.nzb", idx)}, fsutil.SanitizeOptions{})
	if err != nil {
		tb.Fatalf("NewJob: %v", err)
	}
	return job
}

// addWithManifest re-attaches the manifest/progress Add() clears for queued
// jobs once a store is present (queue.go's Add: for a StatusQueued job, once
// q.store != nil or q.stateDir != "", it nils job.manifest and job.progress).
// Without this, PromoteNext would fall back to a real disk read for every
// promoted job in the WithStore variant only, confounding the comparison
// this file exists to make.
func addWithManifest(tb testing.TB, q *Queue, job *Job) {
	tb.Helper()
	manifest, progress := job.manifest, job.progress
	if err := q.Add(job); err != nil {
		tb.Fatalf("Add: %v", err)
	}
	job.manifest, job.progress = manifest, progress
}

// newBenchStore opens a real on-disk SQLite-backed store (WAL mode, via
// history.Open) so the WithStore benchmark variant exercises genuine SQLite
// latency rather than a mock. The caller must call the returned close func
// -- unlike a t.Cleanup-based helper, this is invoked once per benchmark
// iteration (see runPromoteBurstBenchmark), not once per subtest, since a
// benchmark can run thousands of iterations and t.Cleanup would otherwise
// leave that many SQLite connections open simultaneously until the whole
// subtest finished.
func newBenchStore(tb testing.TB, dir string) (*SQLiteStore, func()) {
	tb.Helper()
	db, err := history.Open(tb.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		tb.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)
	return NewSQLiteStore(repo.DB(), dir, repo), func() { _ = db.Close() }
}

// setupPromoteBurst builds a queue with n queued jobs held back from
// promotion (MaxActiveJobs starts at 0), so the later capacity raise
// triggers a single PromoteNext call that bursts through all n candidates --
// the shape #229 flagged as the real contention risk, as opposed to steady
// one-at-a-time promotion. The returned close func releases the SQLite
// connection (and, for withStore, must be called before the directory is
// removed); it is a no-op when withStore is false.
func setupPromoteBurst(tb testing.TB, dir string, n int, withStore bool) (*Queue, func()) {
	tb.Helper()
	var q *Queue
	closeStore := func() {}
	if withStore {
		var store *SQLiteStore
		store, closeStore = newBenchStore(tb, dir)
		q = New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(0))
	} else {
		q = New(WithMaxActiveJobs(0))
	}
	for i := range n {
		addWithManifest(tb, q, buildPromoteCandidate(tb, i))
	}
	return q, closeStore
}

func runPromoteBurstBenchmark(b *testing.B, n int, withStore bool) {
	// b.TempDir() defers its own cleanup to the end of the whole subtest, so
	// calling it once per iteration would leave hundreds of thousands of
	// directories on disk for a benchtime-scaled run. baseDir is created once;
	// each iteration gets its own os.MkdirTemp subdirectory, removed explicitly
	// the same way the store connection is.
	//
	// b.Loop() also expects the timer left running at the end of each
	// iteration's body, so the previous iteration's store and directory are
	// cleaned up during the *next* iteration's (already-stopped) setup phase
	// rather than at the tail of the current one.
	baseDir := b.TempDir()
	var prevClose func()
	var prevDir string
	for b.Loop() {
		b.StopTimer()
		if prevClose != nil {
			prevClose()
		}
		if prevDir != "" {
			if err := os.RemoveAll(prevDir); err != nil {
				b.Fatalf("RemoveAll: %v", err)
			}
		}
		var dir string
		if withStore {
			d, err := os.MkdirTemp(baseDir, "iter")
			if err != nil {
				b.Fatalf("MkdirTemp: %v", err)
			}
			dir = d
		}
		prevDir = dir
		var q *Queue
		q, prevClose = setupPromoteBurst(b, dir, n, withStore)
		b.StartTimer()

		q.SetMaxActiveJobs(n) // raises the cap and calls PromoteNext, bursting all n through in one call
	}
	if prevClose != nil {
		prevClose()
	}
	if prevDir != "" {
		_ = os.RemoveAll(prevDir)
	}
}

// BenchmarkPromoteNext_NilStore measures the RAM-only cost of a promotion
// burst: no store attached, so RestoreJobProgress/Update never run.
func BenchmarkPromoteNext_NilStore(b *testing.B) {
	for _, n := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			runPromoteBurstBenchmark(b, n, false)
		})
	}
}

// BenchmarkPromoteNext_WithStore measures the same burst with a real
// SQLite-backed store attached, so RestoreJobProgress+Update run under q.mu
// for every promoted job. The delta against BenchmarkPromoteNext_NilStore at
// the same N isolates the SQLite-added cost.
func BenchmarkPromoteNext_WithStore(b *testing.B) {
	for _, n := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			runPromoteBurstBenchmark(b, n, true)
		})
	}
}

// TestPromoteNext_ContentionUnderBurst measures whether a promotion burst
// holding q.mu (exclusive) observably stalls a concurrently running
// ForEachUnfinishedArticle scan (shared RLock) -- the actual concurrent
// counterpart in this codebase, since the downloader's dispatch loop calls
// ForEachUnfinishedArticle every pass but nothing calls PromoteNext
// periodically. This reports the measured max stall via t.Logf rather than
// asserting a threshold: this repo has documented CI timing flakiness, and a
// hard-coded latency bound would be exactly that class of flaky test.
func TestPromoteNext_ContentionUnderBurst(t *testing.T) {
	const n = 100
	q, closeStore := setupPromoteBurst(t, t.TempDir(), n, true)
	defer closeStore()

	var (
		maxStallNs atomic.Int64
		scans      atomic.Int64
		stop       atomic.Bool
		wg         sync.WaitGroup
	)
	wg.Go(func() {
		for !stop.Load() {
			start := time.Now()
			q.ForEachUnfinishedArticle(func(UnfinishedArticle) bool { return true })
			elapsed := time.Since(start).Nanoseconds()
			scans.Add(1)
			for {
				cur := maxStallNs.Load()
				if elapsed <= cur || maxStallNs.CompareAndSwap(cur, elapsed) {
					break
				}
			}
		}
	})

	burstStart := time.Now()
	q.SetMaxActiveJobs(n) // bursts all n candidates through PromoteNext while the scan goroutine runs concurrently
	burstElapsed := time.Since(burstStart)

	stop.Store(true)
	wg.Wait()

	t.Logf("promotion burst (n=%d, with store): %v total; concurrent ForEachUnfinishedArticle: %d scans, max single-call stall %v",
		n, burstElapsed, scans.Load(), time.Duration(maxStallNs.Load()))

	if q.ActiveSet().Len() != n {
		t.Fatalf("expected all %d jobs promoted after the burst, got %d active", n, q.ActiveSet().Len())
	}
}
