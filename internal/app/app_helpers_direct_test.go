package app

import (
	"os"
	"path/filepath"
	"strings"
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
	t.Parallel()
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

// TestStageGzFile tests stageGzFile directly: valid staging produces a readable
// gzipped file at the returned path, while an invalid directory fails.
func TestStageGzFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := []byte("hello staging data")

	path, err := stageGzFile(dir, data)
	if err != nil {
		t.Fatalf("stageGzFile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	if !strings.HasPrefix(path, dir) {
		t.Errorf("path %s not in dir %s", path, dir)
	}
	got := readGzFile(t, path)
	if string(got) != string(data) {
		t.Errorf("readGzFile = %q, want %q", got, data)
	}

	// Error path: nonexistent directory
	if _, err := stageGzFile(filepath.Join(dir, "no-such-dir"), data); err == nil {
		t.Error("stageGzFile in nonexistent directory returned nil error, want error")
	}
}
