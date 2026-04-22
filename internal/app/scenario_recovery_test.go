package app_test

import (
	"testing"
)

// TestRecovery_PostProcTrueOnRestart verifies that a job whose PostProc
// flag was persisted but whose history.Add + queue.Remove did not run
// before a crash is finalized on the next startup.
//
// Shape (to be implemented when the skip is removed in B.1):
//  1. Start a harness whose post-proc stage blocks on a caller-controlled
//     channel so we can observe PostProc=true in the queue.
//  2. Enqueue a simple job, wait until its snapshot shows PostProc=true.
//  3. Shutdown without releasing the gate — queue.Remove never runs.
//  4. Re-open the admin dir with a fresh Application and let its startup
//     rescan finalize the stranded job.
//  5. Assert the job lands in history and is no longer in the queue.
//
// EXPECTED FAIL until B.1: Application.Start rescan routes through
// sendToPostProcessor → SetPostProcStarted, which returns (false, nil)
// because PostProc is already true, so the job stays stranded forever.
func TestRecovery_PostProcTrueOnRestart(t *testing.T) {
	t.Skip("EXPECTED FAIL until B.1: startup rescan does not re-drive post-proc for PostProc=true jobs")
}
