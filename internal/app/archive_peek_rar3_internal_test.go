package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestPeek_RAR3VolumeIsSkippedWithoutForkingUnrar pins that the early check
// lists RAR5 only: a RAR3 volume is skipped, and no `unrar` process is started
// for it (rarheader.Inspect would fork one, with no timeout, on the single
// completion goroutine). A recording fake `unrar` is the only one on PATH and
// would, if run, report a member the rules exclude.
//
// Not parallel: it sets PATH for the process.
func TestPeek_RAR3VolumeIsSkippedWithoutForkingUnrar(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "unrar-was-run")
	script := "#!/bin/sh\necho ran > " + marker + "\necho 'Name: payload.txt'\n"
	if err := os.WriteFile(filepath.Join(bin, "unrar"), []byte(script), 0o700); err != nil { //nolint:gosec // test fake executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	rar3, err := os.ReadFile(filepath.Join("..", "rarheader", "testdata", "rar3-comment-plain.rar"))
	if err != nil {
		t.Fatal(err)
	}
	a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false, []peekFile{{"x.rar", rar3}})
	a.complete(t, 0)

	if _, err := os.Stat(marker); err == nil {
		t.Error("the peek forked unrar for a RAR3 volume")
	}
	if got := a.state(t); got != unwanted.StateNone {
		t.Errorf("Unwanted = %d, want none: a RAR3 volume is left to the post-unpack check", got)
	}
	if !a.j.IsComplete() {
		t.Error("the RAR3 file was not marked complete")
	}
}
