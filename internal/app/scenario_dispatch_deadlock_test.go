package app_test

import (
	"testing"
)

// TestDispatch_NoDeadlockWhenCompletionsFull verifies that the downloader
// dispatcher makes forward progress when its emitResult call would block
// (full completions buffer) while it still holds tryMu and the queue
// RLock taken by ForEachUnfinishedArticle.
//
// Shape (to be implemented when the skip is removed in B.2):
//  1. Construct a downloader with a CompletionsBuffer of 1.
//  2. Block the pipeline consumer (e.g. don't start it) so completions
//     backs up after a single send.
//  3. Add a job with enough articles to force at least one all-servers-
//     exhausted emitResult inside the ForEachUnfinishedArticle callback
//     (scripted server returns 430 for every request).
//  4. Verify the dispatcher goroutine is not blocked: a subsequent
//     queue.Add / health check / Stop() completes within a few seconds.
//
// EXPECTED FAIL until B.2: dispatchPass emits inline while holding both
// tryMu and the queue RLock; a full completions channel deadlocks the
// dispatcher because the consumer needs the queue write lock.
func TestDispatch_NoDeadlockWhenCompletionsFull(t *testing.T) {
	t.Skip("EXPECTED FAIL until B.2: emitResult inside held locks deadlocks on full completions buffer")
}
