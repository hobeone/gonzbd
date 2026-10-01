package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestCompleteFinalizedFile_AdmittedJobIsNotReportedDownloaded: a hand-off
// from Fetching (Fail, a hopeless callback) admits the job to post-processing
// while the dispatcher still holds it at Fetching, until the finalizer's
// CancelJob. A file completing in that window makes the job complete, and its
// download-complete report must not be made: Fetching -> Assessing would have
// the tick launch runAssess beside the admitted run.
func TestCompleteFinalizedFile_AdmittedJobIsNotReportedDownloaded(t *testing.T) {
	t.Parallel()
	application, j := fetchingRarApp(t)
	id := j.ID()
	for i := range rarVolumes - 1 {
		completeFile(t, application, id, i)
	}

	// No failure reason, so the held stage keeps the job registered at
	// Fetching: a reason skips the stages, and the finalizer deregisters it.
	application.maybeFinalize(id, "")
	if !application.postProcAdmissions.has(j) {
		t.Fatal("precondition: the hand-off did not admit the job")
	}
	if got := j.Snapshot().State; got.State != job.Fetching || got.Next != job.StateUnset {
		t.Fatalf("precondition: before the late completion %+v, want Fetching with no next", got)
	}

	completeFile(t, application, id, rarVolumes-1)

	if !j.IsComplete() {
		t.Fatal("precondition: the late completion left the job incomplete")
	}
	if got := j.Snapshot().State; got.State != job.Fetching || got.Next != job.StateUnset {
		t.Errorf("a completion after the hand-off reported the admitted job's download: %+v, "+
			"want Fetching with no next — the tick launches a second worker from there", got)
	}
}
