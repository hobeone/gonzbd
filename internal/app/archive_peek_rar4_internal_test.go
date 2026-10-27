package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/testutil"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestPeek_RAR4VolumeIsListedWithoutForkingUnrar pins that the early check
// reads a RAR 1.5-4.x volume in pure Go: one whose members the rules exclude
// blocks the job, and one rarengine cannot list (encrypted headers) is
// skipped. Neither starts an `unrar` process (rarheader.Inspect would fork one,
// with no timeout, on the single completion goroutine). A recording fake
// `unrar` is the only one on PATH and would, if run, report a member the rules
// exclude.
//
// Not parallel: it sets PATH for the process.
func TestPeek_RAR4VolumeIsListedWithoutForkingUnrar(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "unrar-was-run")
	testutil.WriteExecutable(t, filepath.Join(bin, "unrar"),
		"#!/bin/sh\necho ran > "+marker+"\necho 'Name: payload.txt'\n")
	t.Setenv("PATH", bin)

	cases := []struct {
		name    string
		fixture []byte
		want    unwanted.State
	}{
		{"members listed and judged", unpackFixture(t, "rar4_lz.rar"), unwanted.StateBlocked},
		{"encrypted headers skipped", unpackFixture(t, "rar4_encrypted_header.rar"), unwanted.StateNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(marker) // a fork in an earlier case is that case's failure
			a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false, []peekFile{{"x.rar", tc.fixture}})
			a.complete(t, 0)

			if _, err := os.Stat(marker); err == nil {
				t.Error("the peek forked unrar for a RAR 1.5-4.x volume")
			}
			if got := a.state(t); got != tc.want {
				t.Errorf("Unwanted = %d, want %d", got, tc.want)
			}
			if !a.j.IsComplete() {
				t.Error("the RAR 1.5-4.x file was not marked complete")
			}
		})
	}
}
