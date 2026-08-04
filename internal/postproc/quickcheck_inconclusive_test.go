package postproc

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
)

// A par2 scan that fails is not a job with no par2 files, and the two must
// not record the same outcome. FindPar2Files errors when the download
// directory cannot be read, at which point whether this job has par2 sets is
// unknown — reporting NotRun would let the repair stage skip par2 on the
// strength of a question nothing answered.
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
	if job.QuickCheck != QuickCheckInconclusive {
		t.Errorf("QuickCheck = %s after an unreadable download directory, want inconclusive", job.QuickCheck)
	}
}

// The genuinely-empty case must stay NotRun. A directory that reads fine and
// holds no par2 files has nothing to verify, and forcing repair to run a par2
// subprocess for every such job would cost 10-30s each for no information.
func TestQuickCheckStage_NoPar2FilesIsNotRun(t *testing.T) {
	t.Parallel()

	job, _ := stageJob(t)

	stage := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	stage.SetEnabled(true)

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.QuickCheck != QuickCheckNotRun {
		t.Errorf("QuickCheck = %s for a readable directory with no par2 files, want not-run", job.QuickCheck)
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

// TestRepairStage_InconclusiveQuickCheckOverridesDirectUnpackSuccess is the
// third state, and the reason it has to exist.
//
// QuickCheck defends against this correctly inside its own stage: when it
// cannot read the manifest it returns an error *before* recording that it ran,
// because "verification that did not run is indistinguishable from
// verification that passed" (stage_quickcheck.go). But the repair stage then
// reads the same two booleans and rebuilds the confusion one level up —
// "attempted and could not" and "never attempted" were both encoded as
// not-run, and the DirectUnpack shortcut treats not-run as nothing-to-worry-
// about.
//
// The result is a job whose CRCs nothing verified at all: QuickCheck bailed,
// and repair skipped on DirectUnpack's say-so. DirectUnpack only knows that
// rarengine could mechanically walk the archive's entries, not that the source
// data was complete — which is exactly the reasoning that already makes an
// explicit QuickCheck failure override it.
func TestRepairStage_InconclusiveQuickCheckOverridesDirectUnpackSuccess(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyPar2Fixtures(t, dir)

	// DirectUnpack reports a clean sweep: every set extracted, nothing
	// failed, nothing skipped. On its own this is the shortcut's premise.
	job.DirectUnpackSets = map[string]directunpack.SuccessSet{
		"ok": {RarParts: []string{"data.bin"}, ExtractedFiles: []string{"data.out"}},
	}
	// QuickCheck tried and could not finish — it has no verdict to offer,
	// which is not the same as offering a clean one.
	job.QuickCheck = QuickCheckInconclusive

	stage := &RepairStage{
		UseGoPar2: true,
		Log:       slog.New(slog.DiscardHandler),
	}
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, line := range job.OutputLines {
		if line == "[repair] Skipped: Direct Unpack successfully extracted all archives during download" {
			t.Fatalf("repair was skipped on DirectUnpack's say-so after a QuickCheck that could not verify anything; nothing checked this job's CRCs. output: %v", job.OutputLines)
		}
	}
}

// The shortcut must still fire when QuickCheck genuinely had nothing to do —
// disabled, or no par2 sets to check against. That is the case it was built
// for, and narrowing it to nothing would cost every DirectUnpack job an
// expensive par2 subprocess it does not need.
func TestRepairStage_NotRunQuickCheckStillAllowsTheDirectUnpackSkip(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyPar2Fixtures(t, dir)

	job.DirectUnpackSets = map[string]directunpack.SuccessSet{
		"ok": {RarParts: []string{"data.bin"}, ExtractedFiles: []string{"data.out"}},
	}
	job.QuickCheck = QuickCheckNotRun

	stage := &RepairStage{
		UseGoPar2: true,
		Log:       slog.New(slog.DiscardHandler),
	}
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	skipped := false
	for _, line := range job.OutputLines {
		if line == "[repair] Skipped: Direct Unpack successfully extracted all archives during download" {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("repair ran despite a clean DirectUnpack and a QuickCheck that never had anything to check; output: %v", job.OutputLines)
	}
}
