package app

import (
	"strings"
	"testing"
)

// TestFailMsgForJob_IndexIsNotCountedAsRecognizedCapacity pins the
// classification half of the re-key at the job level: a plainly-named ".par2"
// subject does not match the volume-naming convention, so it contributes
// nothing to the recognized recovery figure. The superseded figures counted it,
// because they keyed on a name test matching any ".par2" subject.
//
// What that figure then licenses is a separate question, and a narrower one
// than an earlier version of this test assumed. Zero recognized capacity does
// not establish that a job cannot be repaired — the file may carry recovery
// slices the subject line gives no way to see. The verdict is withheld here
// for that reason; see
// TestFailMsgForJob_UnrecognizedPar2WithholdsTheZeroCapacityVerdict.
func TestFailMsgForJob_IndexIsNotCountedAsRecognizedCapacity(t *testing.T) {
	t.Parallel()
	job := buildFailMsgJob(t, []failMsgFile{
		{subject: "movie.part01.rar", bytes: 1000},
		{subject: "movie.par2", bytes: 100}, // no .volNNN+MM segment
	}, 0)

	if got := job.RecoveryBytes(); got != 0 {
		t.Errorf("RecoveryBytes() = %d, want 0 — the subject does not match the volume "+
			"convention, so its 100 B is not recognized recovery capacity", got)
	}
	if got := job.RecoveryFiles(); got != 0 {
		t.Errorf("RecoveryFiles() = %d, want 0", got)
	}

	if got := failMsgForJob(job); strings.Contains(got, "exceeds repair capacity") {
		t.Errorf("failMsgForJob() = %q, still weighing damage against the index's bytes as "+
			"though they were repair capacity", got)
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
	t.Parallel()
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
