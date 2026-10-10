package app

import (
	"hash/crc32"
	"log/slog"
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// resumedPeekFixture is a one-file job whose only file is a RAR naming a
// flagged member, and the verification outcome that finishes it by path.
func resumedPeekFixture(t *testing.T) (*peekApp, []durability.FileRow, []durability.WrittenRow, verifyResult) {
	t.Helper()
	data := unpackFixture(t, "single_rar5.rar")
	a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false, []peekFile{{"release.part1.rar", data}})
	files := []durability.FileRow{{FileIndex: 0, Filename: "release.part1.rar"}}
	rows := []durability.WrittenRow{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: int64(len(data)), CRC32: crc32.ChecksumIEEE(data)}}
	res := verifyResult{
		Verdicts: []durability.FileVerdict{{FileIdx: 0, SetComplete: true}},
		Verified: map[int][]durability.WrittenRow{0: rows},
	}
	return a, files, rows, res
}

// A file the verifier finishes at hydration is peeked before it is marked
// complete. The mark is what lets the download-complete report take the job to
// Assessing, so a last file marked first can reach post-processing unpeeked.
func TestInstallVerification_PeeksAFinishedFileBeforeMarkingItComplete(t *testing.T) {
	t.Parallel()
	a, files, rows, res := resumedPeekFixture(t)

	var peeked []int
	var completeAtPeek bool
	finished := installVerification(a.j, files, rows, res, true, slog.New(slog.DiscardHandler), func(fi int) {
		peeked = append(peeked, fi)
		completeAtPeek = a.j.Progress().FileComplete(fi)
	})

	if len(peeked) != 1 || peeked[0] != 0 {
		t.Fatalf("peeked = %v, want [0]", peeked)
	}
	if completeAtPeek {
		t.Error("file 0 was already marked complete when it was peeked; a last file marked first reaches Assessing unpeeked")
	}
	if len(finished) != 1 || !a.j.Progress().FileComplete(0) {
		t.Errorf("finished = %v, complete = %v, want file 0 finished and complete", finished, a.j.Progress().FileComplete(0))
	}
}

// The peek the application installs on its residency blocks a job whose last
// file is a flagged archive before that file is complete, so the job is
// Blocked by the time it can be reported download-complete.
func TestInstallVerification_TheResidencyPeekBlocksTheJobBeforeTheMark(t *testing.T) {
	t.Parallel()
	a, files, rows, res := resumedPeekFixture(t)
	if a.residency.peek == nil {
		t.Fatal("the application did not install a peek on its residency")
	}

	var blockedAtPeekEnd unwanted.State
	var completeAtPeekEnd bool
	installVerification(a.j, files, rows, res, true, slog.New(slog.DiscardHandler), func(fi int) {
		a.residency.peek(a.j, fi)
		blockedAtPeekEnd = a.state(t)
		completeAtPeekEnd = a.j.Progress().FileComplete(fi)
	})

	if blockedAtPeekEnd != unwanted.StateBlocked {
		t.Errorf("Unwanted after the peek = %d, want blocked", blockedAtPeekEnd)
	}
	if completeAtPeekEnd {
		t.Error("the file was complete before its peek finished")
	}
}

// The consumer of a Resumed completion does not peek again: the hydration
// already did, ahead of the mark.
func TestCompleteFinalizedFile_ResumedCompletionIsNotPeeked(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false,
		[]peekFile{{"release.part1.rar", unpackFixture(t, "single_rar5.rar")}})
	peeks := 0
	a.peekedHook = func() { peeks++ }

	if err := a.completeFinalizedFile(FileComplete{JobID: a.j.ID(), FileIdx: 0, Resumed: true}); err != nil {
		t.Fatalf("completeFinalizedFile: %v", err)
	}
	if peeks != 0 {
		t.Errorf("a Resumed completion was peeked %d times, want 0", peeks)
	}
	if got := a.state(t); got != unwanted.StateNone {
		t.Errorf("Unwanted = %d, want none: the consumer peeked", got)
	}

	if err := a.completeFinalizedFile(FileComplete{JobID: a.j.ID(), FileIdx: 0}); err != nil {
		t.Fatalf("completeFinalizedFile: %v", err)
	}
	if peeks != 1 || a.state(t) != unwanted.StateBlocked {
		t.Errorf("an assembler completion: peeks = %d, Unwanted = %d, want 1 and blocked", peeks, a.state(t))
	}
}

// A hydration hands each file the verifier finished to residency.peek, ahead
// of the mark: the wiring from Hydrate through installVerification.
func TestHydrate_PeeksEachFileTheVerifierFinishedBeforeItIsMarked(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "hydrate-peek", 2, 2)
	env.writeFileA(t, j, 2, 0, 1)
	a1.recordFileA(t, j.ID(), false, 0, 1)

	var peeked []int
	var completeAtPeek bool
	a2 := env.newApp(t, func(a *Application) {
		a.dispatcher.Pause()
		a.residency.peek = func(pj *job.Job, fi int) {
			peeked = append(peeked, fi)
			completeAtPeek = pj.Progress().FileComplete(fi)
		}
	})
	a2.start(t)
	if err := a2.dispatcher.LoadProgress(t.Context(), j.ID()); err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}

	if !slices.Equal(peeked, []int{0}) {
		t.Fatalf("peeked = %v, want [0]: the verifier finished file A only", peeked)
	}
	if completeAtPeek {
		t.Error("file A was already complete when it was peeked")
	}
	if !a2.registered(t, j.ID()).Progress().FileComplete(0) {
		t.Error("file A is not complete after the hydration")
	}
}
