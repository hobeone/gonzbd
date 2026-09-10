package postproc

import (
	"archive/tar"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// These four helpers were untested before this change and were surfaced by
// check_test_alignment when stage_unpack.go was touched. They are pre-existing
// debt in a file this branch edited for one comment removal, not new code — the
// tests are here because the gate's answer to that is a real test.

// TestUnpackStage_filterPending pins that the processed set is consulted by
// MainFile, which is what stops the recursive unpack passes from re-extracting
// an archive an earlier pass already handled.
func TestUnpackStage_filterPending(t *testing.T) {
	t.Parallel()
	u := &UnpackStage{}

	archives := []unpack.Archive{
		{Name: "a", MainFile: "a.part01.rar"},
		{Name: "b", MainFile: "b.7z"},
		{Name: "c", MainFile: "c.tar"},
	}

	t.Run("nothing processed keeps every archive in order", func(t *testing.T) {
		t.Parallel()
		got := u.filterPending(archives, map[string]bool{})
		if len(got) != 3 {
			t.Fatalf("got %d pending, want 3", len(got))
		}
		for i := range got {
			if got[i].MainFile != archives[i].MainFile {
				t.Errorf("pending[%d] = %q, want %q — order must be preserved", i, got[i].MainFile, archives[i].MainFile)
			}
		}
	})

	t.Run("a processed MainFile is dropped", func(t *testing.T) {
		t.Parallel()
		got := u.filterPending(archives, map[string]bool{"b.7z": true})
		if len(got) != 2 {
			t.Fatalf("got %d pending, want 2", len(got))
		}
		for _, a := range got {
			if a.MainFile == "b.7z" {
				t.Error("an archive whose MainFile is marked processed was still pending")
			}
		}
	})

	t.Run("a false value does not count as processed", func(t *testing.T) {
		t.Parallel()
		// The map is a set-with-absent-default; an explicit false must behave
		// like a missing key, or a caller clearing an entry would silently
		// skip the archive.
		if got := u.filterPending(archives, map[string]bool{"b.7z": false}); len(got) != 3 {
			t.Errorf("got %d pending, want 3", len(got))
		}
	})

	t.Run("all processed yields none", func(t *testing.T) {
		t.Parallel()
		all := map[string]bool{"a.part01.rar": true, "b.7z": true, "c.tar": true}
		if got := u.filterPending(archives, all); len(got) != 0 {
			t.Errorf("got %d pending, want 0: %+v", len(got), got)
		}
	})
}

// TestUnpackStage_handleDirectUnpack pins the handoff from DirectUnpack: the
// parts it already extracted must be marked processed so the unpack stage does
// not extract them a second time, and its output files must be owned so the
// cleanup stages do not skip them as foreign.
//
// Parts are marked under BOTH the full path and the basename, because Scan's
// lookup is basename-based while the recursion tracks full paths. Marking only
// one leaves DirectUnpack's work re-extracted.
func TestUnpackStage_handleDirectUnpack(t *testing.T) {
	t.Parallel()
	u := &UnpackStage{}

	dir := t.TempDir()
	extracted := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(extracted, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	job := &Job{
		Job:         newQueueJob(t, "dujob", 0),
		DownloadDir: dir,
		// Non-nil: markOwned returns early on a nil OwnedFiles, so with nil
		// nothing is recorded and the ownership assertion below fails. That
		// failure would be about the fixture rather than about
		// handleDirectUnpack, which is the reading a later maintainer would
		// have to disprove before trusting the test.
		OwnedFiles: map[string]struct{}{},
		DirectUnpackSets: map[string]directunpack.SuccessSet{
			"set-one": {
				ExtractedFiles: []string{extracted},
				RarParts:       []string{filepath.Join(dir, "set-one.part01.rar")},
			},
		},
	}

	processed := map[string]bool{}
	var successful []unpack.Archive
	u.handleDirectUnpack(t.Context(), slog.New(slog.DiscardHandler), job, processed, &successful)

	if !processed[filepath.Join(dir, "set-one.part01.rar")] {
		t.Error("the DirectUnpack part was not marked processed by full path")
	}
	if !processed["set-one.part01.rar"] {
		t.Error("the DirectUnpack part was not marked processed by basename; Scan looks it up that way")
	}
	if len(successful) != 1 {
		t.Fatalf("recorded %d successful archives, want 1", len(successful))
	}
	if successful[0].Type != unpack.RarArchive {
		t.Errorf("archive type = %v, want RarArchive", successful[0].Type)
	}
	if _, owned := job.OwnedFiles[extracted]; !owned {
		t.Errorf("%s was not marked owned; the cleanup stages skip unowned files", extracted)
	}
}

// TestUnpackStage_handleDirectUnpack_NoSets pins that the no-op path touches
// nothing. An empty DirectUnpackSets is the ordinary case for a job whose
// archives were not streamed during download.
func TestUnpackStage_handleDirectUnpack_NoSets(t *testing.T) {
	t.Parallel()
	u := &UnpackStage{}
	job := &Job{Job: newQueueJob(t, "nodujob", 0), DownloadDir: t.TempDir()}

	processed := map[string]bool{}
	var successful []unpack.Archive
	u.handleDirectUnpack(t.Context(), slog.New(slog.DiscardHandler), job, processed, &successful)

	if len(processed) != 0 || len(successful) != 0 {
		t.Errorf("processed=%v successful=%v; want both empty", processed, successful)
	}
}

// TestRecordUnpackFailure pins what a failed extraction leaves behind: the job
// flagged, the FIRST error kept rather than the last, and the diagnostics a
// user sees in the history entry.
func TestRecordUnpackFailure(t *testing.T) {
	t.Parallel()

	t.Run("flags the job and keeps the first error", func(t *testing.T) {
		t.Parallel()
		job := &Job{Job: newQueueJob(t, "failjob", 0)}
		a := unpack.Archive{Name: "movie.rar", Type: unpack.RarArchive}

		earlier := errors.New("the earlier failure")
		firstErr := error(nil)
		recordUnpackFailure(t.Context(), slog.New(slog.DiscardHandler), job, a,
			unpack.Result{}, earlier, &firstErr)

		if !job.UnpackError {
			t.Error("UnpackError was not set")
		}
		if !errors.Is(firstErr, earlier) {
			t.Errorf("firstErr = %v, want it to wrap the underlying error", firstErr)
		}

		// A second failure must not displace the first — the caller reports
		// firstErr, and the earliest failure is the one that explains the rest.
		later := errors.New("the later failure")
		recordUnpackFailure(t.Context(), slog.New(slog.DiscardHandler), job, a,
			unpack.Result{}, later, &firstErr)
		if errors.Is(firstErr, later) {
			t.Error("a later failure overwrote the first error")
		}
	})

	t.Run("names the engine and includes command output", func(t *testing.T) {
		t.Parallel()
		job := &Job{Job: newQueueJob(t, "failjob2", 0)}
		a := unpack.Archive{Name: "movie.rar", Type: unpack.RarArchive}
		res := unpack.Result{
			Engine:      "go_unrar",
			CommandLine: "unrar x movie.rar",
			Output:      "CRC failed\nbroken",
		}
		firstErr := error(nil)
		recordUnpackFailure(t.Context(), slog.New(slog.DiscardHandler), job, a,
			res, errors.New("boom"), &firstErr)

		joined := strings.Join(job.OutputLines, "\n")
		for _, want := range []string{"go_unrar", "movie.rar", "(FAILED)", "Command: unrar x movie.rar", "CRC failed", "broken"} {
			if !strings.Contains(joined, want) {
				t.Errorf("OutputLines missing %q:\n%s", want, joined)
			}
		}
	})

	t.Run("falls back to the archive type when the engine is unset", func(t *testing.T) {
		t.Parallel()
		job := &Job{Job: newQueueJob(t, "failjob3", 0)}
		a := unpack.Archive{Name: "movie.tar", Type: unpack.TarArchive}
		firstErr := error(nil)
		recordUnpackFailure(t.Context(), slog.New(slog.DiscardHandler), job, a,
			unpack.Result{}, errors.New("boom"), &firstErr)

		if joined := strings.Join(job.OutputLines, "\n"); !strings.Contains(joined, "[tar]") {
			t.Errorf("OutputLines = %q; want the archive type as the engine name", joined)
		}
	})
}

// TestExtractTarArchive pins the tar path end to end, including that the
// per-engine OnOutput callback is wired — that callback is what the history
// entry and the UI show for the stage, so a silent extraction looks to a user
// like nothing happened.
func TestExtractTarArchive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	tarPath := filepath.Join(dir, "bundle.tar")
	f, err := os.Create(tarPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	body := []byte("tar member contents")
	if err := tw.WriteHeader(&tar.Header{Name: "inner.txt", Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var lines []string
	job := &Job{
		Job:         newQueueJob(t, "tarjob", 0),
		DownloadDir: dir,
		OnOutput: func(tool, line string) {
			lines = append(lines, fmt.Sprintf("%s|%s", tool, line))
		},
	}
	a := unpack.Archive{Name: "bundle.tar", Type: unpack.TarArchive, MainFile: tarPath}

	res, err := extractTarArchive(t.Context(), slog.New(slog.DiscardHandler), job, a, unpack.Options{})
	if err != nil {
		t.Fatalf("extractTarArchive: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("result error: %v", res.Err)
	}

	got, rErr := os.ReadFile(filepath.Join(dir, "inner.txt"))
	if rErr != nil {
		t.Fatalf("the tar member was not extracted: %v", rErr)
	}
	if string(got) != string(body) {
		t.Errorf("extracted content = %q, want %q", got, body)
	}

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "go_tar|") {
		t.Errorf("no output was attributed to go_tar:\n%s", joined)
	}
}
