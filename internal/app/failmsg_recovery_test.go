package app

import (
	"strings"
	"testing"
)

// TestFailMsgForJob_IndexOnlyHasNoRepairCapacity pins the one job shape whose
// verdict this re-key changes.
//
// A job carrying a par2 index but no recovery volumes has no repair capacity
// at all: the index holds per-file checksums, not recovery blocks. The
// superseded figures counted it anyway, because they keyed on a name test
// matching any ".par2" subject, so such a job reported the index's bytes as
// capacity and took the "exceeds repair capacity" branch. It now correctly
// reports none.
//
// The distinction is not cosmetic. The same figure drives both abort gates,
// and overstating it is the direction that keeps a genuinely unrepairable job
// in the download queue.
func TestFailMsgForJob_IndexOnlyHasNoRepairCapacity(t *testing.T) {
	job := buildFailMsgJob(t, []failMsgFile{
		{subject: "movie.part01.rar", bytes: 1000},
		{subject: "movie.par2", bytes: 100}, // index: no .volNNN+MM
	}, 0)

	got := failMsgForJob(job)

	if !strings.Contains(got, "no par2 recovery volumes available") {
		t.Errorf("failMsgForJob() = %q, want the no-recovery-volumes verdict.\n"+
			"A bare index is not repair capacity; counting its 100 B would take the "+
			"'exceeds repair capacity' branch instead, which is the pre-re-key behaviour.", got)
	}
	if strings.Contains(got, "exceeds repair capacity") {
		t.Errorf("failMsgForJob() = %q, still reporting the index as repair capacity", got)
	}
}

// TestFailMsgForJob_AgreesWithDispatcherOnPartialFailure is the reason the
// re-key ships as one commit rather than several.
//
// Two independent gates decide "beyond repair" from the same quantity:
// failMsgForJob here, and the downloader's Early Health Gate, which compares
// UnfinishedArticle.RecoveryBytes against failed bytes. Moving one without the
// other opens a window where they disagree, and the disagreement is not
// benign: app.go's OnJobHopeless passes failMsgForJob's result straight into
// maybeFinalize with no fallback string, so a job the dispatcher declares
// hopeless while failMsgForJob still considers it repairable is finalized
// with an *empty* reason — no message for the user at all.
//
// The fixture is chosen to sit in the only band where the two denominators
// disagree, recoveryBytes < failedBytes <= par2Bytes:
//
//	content 1000 B + index 50 B + one recovery volume 100 B, failing 120 B
//
//	  old denominator (index counted): 150 B -> 120 <= 150 -> "" (repairable)
//	  new denominator (volumes only):  100 B -> 120 >  100 -> beyond repair
//
// An index-only fixture cannot pin this: it yields a non-empty message under
// both denominators, so it would pass on unpatched code and on a half-applied
// change alike.
func TestFailMsgForJob_AgreesWithDispatcherOnPartialFailure(t *testing.T) {
	job := buildFailMsgJob(t, []failMsgFile{
		{subject: "movie.part01.rar", bytes: 1000},
		{subject: "movie.par2", bytes: 50},           // index: content, not capacity
		{subject: "movie.vol01+02.par2", bytes: 100}, // the only real capacity
		{subject: "movie.part02.rar", bytes: 120},
	}, 3)

	// Fixture guards. Both read live values from the job — comparing the
	// three constants against each other would fold away at compile time and
	// guard nothing.
	const wantFailed, wantRecovery, oldPar2 = int64(120), int64(100), int64(150)
	if got := job.RecoveryBytes(); got != wantRecovery {
		t.Fatalf("fixture guard: RecoveryBytes() = %d, want %d — the index or a volume "+
			"is being classified differently than this test assumes", got, wantRecovery)
	}
	if got := job.Progress().FailedBytes(); got != wantFailed {
		t.Fatalf("fixture guard: FailedBytes() = %d, want %d — the failure is no longer "+
			"landing where this test needs it", got, wantFailed)
	}
	// The band is what makes the test able to see a split at all: below
	// wantRecovery both denominators say repairable, above oldPar2 both say
	// hopeless, and only in between do they disagree.
	if !(wantRecovery < wantFailed && wantFailed <= oldPar2) { //nolint:staticcheck // QF1001: the band reads as a band, not as De Morgan's inverse
		t.Fatalf("fixture guard: %d < %d <= %d does not hold, so the two denominators "+
			"would agree and this test could not detect a split", wantRecovery, wantFailed, oldPar2)
	}

	got := failMsgForJob(job)

	// The dispatcher's gate is `failedBytes > RecoveryBytes`, which is true
	// here. failMsgForJob must reach the same verdict, or OnJobHopeless
	// finalizes the job with an empty reason.
	if got == "" {
		t.Error("failMsgForJob() returned the empty message, but the dispatcher's Early Health " +
			"Gate declares this job hopeless on the same figures. app.go's OnJobHopeless has no " +
			"fallback string, so this job would be finalized with no reason shown to the user.")
	}
	if !strings.Contains(got, "exceeds repair capacity") {
		t.Errorf("failMsgForJob() = %q, want the exceeds-repair-capacity verdict", got)
	}
}
