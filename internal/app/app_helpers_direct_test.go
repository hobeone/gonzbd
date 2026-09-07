package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
)

// Direct tests for two unexported helpers in app.go, added for the same reason
// as internal/job/progress_helpers_direct_test.go: a comment-only edit to
// app.go put every unexported helper in it on check_test_alignment's bar,
// which is the gate's documented whole-file scope rather than a misfire.
// AGENTS.md names app.go as a file where exactly this happens.

// TestUniqueName_SuffixesUntilFree pins the collision walk. The interesting
// property is that it starts at .1 rather than .0 and keeps going past the
// first collision — a loop that stopped after one attempt would pass a
// single-collision test and fail on disk the first time two names collided.
func TestUniqueName_SuffixesUntilFree(t *testing.T) {
	t.Run("free name is returned unchanged", func(t *testing.T) {
		got := uniqueName("movie", func(string) bool { return false })
		if got != "movie" {
			t.Errorf("uniqueName = %q for a free base, want %q", got, "movie")
		}
	})

	t.Run("first collision takes .1", func(t *testing.T) {
		taken := map[string]bool{"movie": true}
		got := uniqueName("movie", func(n string) bool { return taken[n] })
		if got != "movie.1" {
			t.Errorf("uniqueName = %q, want %q", got, "movie.1")
		}
	})

	t.Run("walks past consecutive collisions", func(t *testing.T) {
		taken := map[string]bool{"movie": true, "movie.1": true, "movie.2": true}
		got := uniqueName("movie", func(n string) bool { return taken[n] })
		if got != "movie.3" {
			t.Errorf("uniqueName = %q, want %q — the loop must keep incrementing, not stop "+
				"after the first suffix", got, "movie.3")
		}
	})
}

// testHistoryRepo opens a real SQLite history store in a temp dir. The query
// path in historyFileProgress needs one; a nil repo takes the early return and
// exercises nothing.
func testHistoryRepo(t *testing.T) *history.Repository {
	t.Helper()
	db, err := history.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return history.NewRepository(db)
}

// TestHistoryFileProgress_NoRepoIsNotAnError pins the early return: a
// store-less Application answers (nil, nil) rather than panicking or erroring.
//
// It uses a bare &Application{} rather than newTestApplication, because the
// branch under test is reached before anything else on the struct is touched
// and the fixture would otherwise allocate three temp dirs and probe the
// filesystem for sparse support to test one nil check.
//
// What this does NOT establish, stated because the obvious reading is wrong:
// it is not a caller fallback. RetryHistoryJob — the only caller — opens with
// `app.historyRepo.Get(ctx, jobID)` (app.go) and would panic on a nil repo
// long before reaching historyFileProgress. The guard is defence inside the
// helper, unreachable from production today.
func TestHistoryFileProgress_NoRepoIsNotAnError(t *testing.T) {
	app := &Application{}

	got, err := app.historyFileProgress(context.Background(), "j1")
	if err != nil {
		t.Errorf("historyFileProgress with no history repo returned %v, want nil — a missing "+
			"store is not a query failure", err)
	}
	if got != nil {
		t.Errorf("historyFileProgress with no history repo returned %d rows, want none", len(got))
	}
}

// TestHistoryFileProgress_ReturnsNothingForAnUnknownJob exercises the query
// itself against a real store: a job with no history_job_files rows yields no
// retained files and no error, which is what lets a first-time retry proceed.
//
// An earlier version of this test guarded on `app.historyRepo == nil` and
// skipped. newTestApplication builds its Application with New(cfg, nil), so
// that guard was true on every run and the query never executed — a test that
// satisfied check_test_alignment while verifying nothing.
func TestHistoryFileProgress_ReturnsNothingForAnUnknownJob(t *testing.T) {
	app := &Application{historyRepo: testHistoryRepo(t)}

	got, err := app.historyFileProgress(context.Background(), "no-such-job")
	if err != nil {
		t.Fatalf("historyFileProgress for an unknown job: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("historyFileProgress returned %d rows for an unknown job, want 0", len(got))
	}
}
