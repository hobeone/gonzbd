package app_test

import (
	"testing"
)

// TestCheckpoint_SurvivesCrashMidDownload verifies that a crash mid-job
// loses at most one checkpoint interval of per-article/per-file progress
// rather than the entire in-memory state.
//
// Shape (to be implemented when the skip is removed in B.4):
//  1. Enqueue a multi-article job.
//  2. Let enough articles complete that the periodic checkpoint would
//     have fired (default 30s; B.4 should make the interval injectable
//     so the test can run in <1s).
//  3. Kill the Application without calling Shutdown (skip the graceful
//     queue.Save path).
//  4. Re-open the admin dir and assert that the queue snapshot reflects
//     the progress observed before the crash (articles marked Done,
//     files marked Complete where applicable).
//
// EXPECTED FAIL until B.4: queue.Save only runs from Shutdown, so the
// reopened queue shows zero progress on all articles.
func TestCheckpoint_SurvivesCrashMidDownload(t *testing.T) {
	t.Skip("EXPECTED FAIL until B.4: queue state is only persisted on Shutdown")
}
