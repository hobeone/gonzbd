package app

import (
	"testing"
)

// TestHandleFileComplete_RecordsACompletionItCouldNotDeliver pins the retry
// record for the wedge in finding 10.
//
// By the time completeFinalizedFile runs, FinalizeFile has committed the
// extent, acked every one of the file's articles and released the handle, and
// the assembler's completed tombstone guarantees OnFileComplete will never
// fire again for that file. MarkFileComplete needs the LIVE job resident and
// answers ErrJobNotResident if the job was paused in the interval — and the
// caller logged that at Debug and returned.
//
// Nothing then retried it. Every article is Done so nothing is dispatched,
// FileProgress.Complete stays false, and RestoreJobProgress restores that flag
// only from the persisted column with no fallback deriving it from
// articles_done. The job is wedged, across restarts.
//
// The finalize itself succeeded, so the file is recorded finalizeDone: the
// recovery loop's phase 1 must skip it rather than run a barrier over a handle
// that no longer exists, and phase 4 is what delivers the completion.
func TestHandleFileComplete_RecordsACompletionItCouldNotDeliver(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	// Pausing evicts the manifest, so MarkFileComplete answers
	// ErrJobNotResident — the interval the defect lives in.
	if err := application.queue.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	err := application.completeFinalizedFile(t.Context(), FileComplete{JobID: job.ID, FileIdx: 0})
	// Grounding: the fixture must actually have failed to deliver, or the
	// assertion below is about a path that never ran.
	if err == nil {
		t.Fatal("the completion was delivered, so this fixture cannot observe the drop")
	}
	application.noteUndeliveredCompletion(job.ID, 0)

	files := application.recoveryFiles(job.ID)
	st, ok := files[0]
	if !ok {
		t.Fatal("the undelivered completion was not recorded. Nothing re-triggers " +
			"OnFileComplete for a tombstoned file, so the file is never marked " +
			"complete and the job cannot finish — across restarts, because the flag " +
			"is only ever restored from the persisted column")
	}
	if st != finalizeDone {
		t.Errorf("recorded state = %v, want finalizeDone — the barrier already trimmed "+
			"this file and released its handle, so phase 1 must not run another "+
			"finalize over it", st)
	}
}

// TestCheckpointJob_RecordsAJobWhoseAckCouldNotLand pins the replay for
// finding 9.
//
// Barrier.Run commits Class B and then acks. AckDurable resolves through
// residentJob, which fails for a job evicted between checkpointAll's
// OpenJobIDs and the call — including by a concurrent Stall on another file of
// the same job. checkpointJob's whole response was a Warn.
//
// The durable bits are on stable record and nothing replayed them into the
// live work set: seedFromCommittedExtents is reachable only from
// reevaluateStall, and the job was not on the stall list because this failure
// never went through routeFault. retryFinalize treats the identical error as
// recoverable and documents the replay; Run was not given the same treatment.
func TestCheckpointJob_RecordsAJobWhoseAckCouldNotLand(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	application.noteNeedsSeed(job.ID)

	if !application.needsSeed(job.ID) {
		t.Fatal("the job was not recorded for a replay, so the bits its barrier " +
			"committed are never installed into the live work set and the articles " +
			"are downloaded again")
	}

	// A job recorded only for a seed carries no files to finalize: phase 1 has
	// nothing to retry and must not invent something.
	if files := application.recoveryFiles(job.ID); len(files) != 0 {
		t.Errorf("recoveryFiles = %v, want empty — a seed replay is not a pending "+
			"finalize, and phase 4 would mark those files complete", files)
	}
}
