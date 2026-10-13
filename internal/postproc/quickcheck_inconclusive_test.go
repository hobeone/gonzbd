package postproc

import (
	"log/slog"
	"path/filepath"
	"testing"
)

// A par2 scan that fails is not a job with no par2 files, and the two must
// not record the same outcome. FindPar2Files errors when the download
// directory cannot be read, at which point whether this job has par2 sets is
// unknown, and reporting NotRun would claim an answer nothing gave.
func TestQuickCheckStage_UnreadableDownloadDirIsInconclusive(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	job.DownloadDir = filepath.Join(dir, "does-not-exist")

	stage := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	stage.SetEnabled(true)

	// Non-fatal by design: par2 repair independently reports what it cannot
	// find, so the stage returns nil and lets the pipeline continue.
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.quickCheck != QuickCheckInconclusive {
		t.Errorf("QuickCheck = %s after an unreadable download directory, want inconclusive", job.quickCheck)
	}
}

// The genuinely-empty case must stay NotRun. A directory that reads fine and
// holds no par2 files has nothing to verify, and Inconclusive would report a
// verification that could not complete where none was possible.
func TestQuickCheckStage_NoPar2FilesIsNotRun(t *testing.T) {
	t.Parallel()

	job, _ := stageJob(t)

	stage := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	stage.SetEnabled(true)

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.quickCheck != QuickCheckNotRun {
		t.Errorf("QuickCheck = %s for a readable directory with no par2 files, want not-run", job.quickCheck)
	}
}

// Every outcome must render as something a reader can act on. A bare integer
// in a repair-stage log line ("QuickCheck reported 3") is worse than no line,
// and an unnamed value silently rendering as "unknown" is how a new state
// would slip past the switch in stage_repair.go.
func TestQuickCheckOutcome_String(t *testing.T) {
	t.Parallel()

	want := map[QuickCheckOutcome]string{
		QuickCheckNotRun:       "not-run",
		QuickCheckClean:        "clean",
		QuickCheckDamaged:      "damaged",
		QuickCheckInconclusive: "inconclusive",
		QuickCheckUnidentified: "unidentified",
	}
	for outcome, s := range want {
		if got := outcome.String(); got != s {
			t.Errorf("QuickCheckOutcome(%d).String() = %q, want %q", int(outcome), got, s)
		}
	}
	if got := QuickCheckOutcome(99).String(); got != "unknown" {
		t.Errorf("an unnamed outcome renders as %q, want %q", got, "unknown")
	}
	// The zero value has to be the one that claims nothing: a Job built
	// without the quickcheck stage running must not read as verified.
	if (QuickCheckOutcome(0)) != QuickCheckNotRun {
		t.Error("the zero value is not QuickCheckNotRun; a job that skipped the stage would carry some other verdict")
	}
}
