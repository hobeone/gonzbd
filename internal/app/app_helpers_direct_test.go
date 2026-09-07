package app

import (
	"context"
	"testing"
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

// TestHistoryFileProgress_NoRepoIsNotAnError pins the early return. It matters
// because the caller (RetryHistoryJob) distinguishes "no retained progress" —
// a legitimate answer that means retry from scratch — from a query failure,
// and a store-less Application must produce the former.
func TestHistoryFileProgress_NoRepoIsNotAnError(t *testing.T) {
	app := newTestApplication(t)
	app.historyRepo = nil

	got, err := app.historyFileProgress(context.Background(), "j1")
	if err != nil {
		t.Errorf("historyFileProgress with no history repo returned %v, want nil — a missing "+
			"store is not a query failure", err)
	}
	if got != nil {
		t.Errorf("historyFileProgress with no history repo returned %d rows, want none", len(got))
	}
}

// TestHistoryFileProgress_ReturnsNothingForAnUnknownJob pins the query path
// itself against a real store: a job with no history_job_files rows yields no
// retained files and no error, which is what lets a first-time retry proceed.
func TestHistoryFileProgress_ReturnsNothingForAnUnknownJob(t *testing.T) {
	app := newTestApplication(t)
	if app.historyRepo == nil || app.historyRepo.DB() == nil {
		t.Skip("test application has no history repo; the query path needs one")
	}

	got, err := app.historyFileProgress(context.Background(), "no-such-job")
	if err != nil {
		t.Fatalf("historyFileProgress for an unknown job: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("historyFileProgress returned %d rows for an unknown job, want 0", len(got))
	}
}
