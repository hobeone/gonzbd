package postproc

import (
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/par2"
)

// #314: verifyJobCRCs' opening guard returned without recording an outcome,
// leaving the zero value QuickCheckNotRun — "there was nothing to verify" —
// on a job that has par2 sets nothing has verified.
//
// The guard is reachable only when sets is non-empty: Run has already
// returned at len(sets) == 0 by the time it calls this. So a job with par2
// sets on disk and an empty or absent queue manifest recorded consent to skip
// verification. Note what the guard was doing: without it, control reaches the
// CRC switch's default arm, which already records Inconclusive. The guard was
// the only thing converting "could not verify" back into "nothing to verify".
// The fix lives in Run, so this pins the half of the contract verifyJobCRCs
// owns: given the Inconclusive its caller established, it must not widen that
// back out on a path where it verified nothing. Reassigning NotRun in this
// guard would be the exact regression, and it is the shape the code had.
func TestVerifyJobCRCs_EmptyManifestWithPar2SetsIsNotConsent(t *testing.T) {
	t.Parallel()

	job, _ := stageJob(t)
	if job.Queue.NumFiles() != 0 {
		t.Fatalf("fixture guard: expected a job with no manifest files, got %d", job.Queue.NumFiles())
	}
	// The precondition Run establishes before it ever calls this.
	job.QuickCheck = QuickCheckInconclusive

	q := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	// Non-empty sets, matching the only way Run reaches this call.
	if err := q.verifyJobCRCs(t.Context(), slog.New(slog.DiscardHandler), job, []par2.Set{{Name: "data"}}); err != nil {
		t.Fatalf("verifyJobCRCs: %v", err)
	}

	if job.QuickCheck != QuickCheckInconclusive {
		t.Errorf("QuickCheck = %s for a job with par2 sets and no manifest to check them against; anything but inconclusive claims a verification that did not happen", job.QuickCheck)
	}
}

// The consequence, stated where a user would feel it: such a job must not take
// the DirectUnpack shortcut.
func TestQuickCheckStage_EmptyManifestJobStillGetsPar2(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyPar2Fixtures(t, dir)
	job.DirectUnpackSets = map[string]directunpack.SuccessSet{
		"ok": {RarParts: []string{"data.bin"}, ExtractedFiles: []string{"data.out"}},
	}

	qc := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	qc.SetEnabled(true)
	if err := qc.Run(t.Context(), job); err != nil {
		t.Fatalf("QuickCheckStage.Run: %v", err)
	}

	repair := &RepairStage{UseGoPar2: true, Log: slog.New(slog.DiscardHandler)}
	if err := repair.Run(t.Context(), job); err != nil {
		t.Fatalf("RepairStage.Run: %v", err)
	}
	for _, line := range job.OutputLines {
		if line == "[repair] Skipped: Direct Unpack successfully extracted all archives during download" {
			t.Fatalf("par2 was skipped for a job whose CRCs nothing verified; QuickCheck recorded %s. output: %v", job.QuickCheck, job.OutputLines)
		}
	}
}

// The inversion #314 asks for, stated directly: once quickcheck knows par2
// sets exist, every path out of the stage that has not earned a verdict must
// leave Inconclusive rather than the zero value.
//
// This is the property that survives the next early return someone adds. The
// two tests above pin today's known paths; this one pins the default those
// paths inherit, so a new `return` fails safe by construction instead of
// depending on review to catch it.
func TestQuickCheckStage_Par2SetsPresentNeverLeavesNotRun(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyPar2Fixtures(t, dir)

	q := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	q.SetEnabled(true)
	if err := q.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.QuickCheck == QuickCheckNotRun {
		t.Errorf("QuickCheck = not-run after the stage found par2 sets in %s; not-run must mean the stage was disabled or found nothing", dir)
	}
}
