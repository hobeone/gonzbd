package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// renameFixture adds two jobs, "first" and "second", and returns the app
// with the ID of "first".
func renameFixture(t *testing.T) (*Application, string) {
	t.Helper()
	a := newTestApplication(t)
	var firstID string
	for _, name := range []string{"first", "second"} {
		parsed := &nzb.NZB{Files: []nzb.File{{
			Subject:  name + ".bin",
			Bytes:    100,
			Articles: []nzb.Article{{ID: name + "-0@t", Bytes: 100, Number: 1}},
		}}}
		j, hdr, raw := buildTestIngestJob(t, a, parsed, name)
		if err := a.AddJob(t.Context(), j, hdr, raw, false); err != nil {
			t.Fatalf("AddJob(%s): %v", name, err)
		}
		if name == "first" {
			firstID = j.ID()
		}
	}
	return a, firstID
}

func nameOf(t *testing.T, a *Application, id string) string {
	t.Helper()
	row, ok := a.Dispatcher().Row(id)
	if !ok {
		t.Fatalf("job %s is gone", id)
	}
	return row.Header.Name
}

// TestRenameJob_RefusesNamesThatAreNoDirectory pins that a rename which
// would leave no name, or name the download directory or its parent, is
// refused and leaves the job's name alone.
func TestRenameJob_RefusesNamesThatAreNoDirectory(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "   ", ".", "..", "...", " . "} {
		a, id := renameFixture(t)
		if _, err := a.RenameJob(id, name); !errors.Is(err, ErrInvalidJobName) {
			t.Errorf("RenameJob(%q) = %v, want ErrInvalidJobName", name, err)
		}
		if got := nameOf(t, a, id); got != "first" {
			t.Errorf("after a refused RenameJob(%q) the name is %q", name, got)
		}
	}
}

// TestRenameJob_GivesASafeUniqueName pins that a rename is sanitised as an
// ingest name is and made unique the way AddJob makes it: never a path
// separator, never another job's name, never a name already on disk.
func TestRenameJob_GivesASafeUniqueName(t *testing.T) {
	t.Parallel()

	t.Run("path separators", func(t *testing.T) {
		t.Parallel()
		a, id := renameFixture(t)
		got, err := a.RenameJob(id, "a/b")
		if err != nil {
			t.Fatalf("RenameJob: %v", err)
		}
		if strings.ContainsAny(got, `/\`) || got != nameOf(t, a, id) {
			t.Errorf("RenameJob(a/b) = %q (stored %q), want one path component", got, nameOf(t, a, id))
		}
	})

	t.Run("another job's name", func(t *testing.T) {
		t.Parallel()
		a, id := renameFixture(t)
		got, err := a.RenameJob(id, "second")
		if err != nil {
			t.Fatalf("RenameJob: %v", err)
		}
		if got == "second" {
			t.Error("the rename took another queued job's name; the two would share a download directory")
		}
	})

	t.Run("a name already on disk", func(t *testing.T) {
		t.Parallel()
		a, id := renameFixture(t)
		if err := os.MkdirAll(filepath.Join(a.config.GetGeneral().DownloadDir, "taken"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := a.RenameJob(id, "taken")
		if err != nil {
			t.Fatalf("RenameJob: %v", err)
		}
		if got == "taken" {
			t.Error("the rename took a name that exists in the download directory")
		}
	})

	t.Run("its own name", func(t *testing.T) {
		t.Parallel()
		a, id := renameFixture(t)
		got, err := a.RenameJob(id, "first")
		if err != nil || got != "first" {
			t.Errorf("RenameJob to its own name = %q, %v; want first, nil", got, err)
		}
	})
}

// TestRenameJob_RefusesAJobWhoseDownloadHasStarted pins that the registry's
// refusal reaches RenameJob's caller as dispatch.ErrJobStarted and leaves
// the name alone.
func TestRenameJob_RefusesAJobWhoseDownloadHasStarted(t *testing.T) {
	t.Parallel()
	a, id := renameFixture(t)
	j, ok := a.Dispatcher().Job(id)
	if !ok {
		t.Fatalf("job %s is gone", id)
	}
	if err := j.MarkJobStarted(time.Now()); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if _, err := a.RenameJob(id, "elsewhere"); !errors.Is(err, dispatch.ErrJobStarted) {
		t.Errorf("RenameJob after a download began = %v, want ErrJobStarted", err)
	}
	if got := nameOf(t, a, id); got != "first" {
		t.Errorf("a refused rename changed the name to %q", got)
	}
}

// TestRenameJob_UnknownJob reports the dispatcher's not-found error.
func TestRenameJob_UnknownJob(t *testing.T) {
	t.Parallel()
	a, _ := renameFixture(t)
	if _, err := a.RenameJob("nope", "x"); !errors.Is(err, dispatch.ErrNotFound) {
		t.Errorf("RenameJob(nope) = %v, want ErrNotFound", err)
	}
}
