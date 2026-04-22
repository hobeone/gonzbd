package app_test

import (
	"testing"
)

// TestRetry_ResetsDownloadStats verifies that RetryHistoryJob clears the
// transient download bookkeeping so post-proc duration math and per-server
// byte counters start fresh on the retried attempt.
//
// Shape (to be implemented when the skip is removed in B.3):
//  1. Run a job end-to-end so it lands in history with non-zero
//     DownloadStarted and ServerStats.
//  2. Call RetryHistoryJob(jobID).
//  3. Snapshot the requeued job and assert DownloadStarted.IsZero() and
//     len(ServerStats) == 0.
//
// EXPECTED FAIL until B.3: RetryHistoryJob in app.go resets Status,
// PostProc, FailedBytes, and per-article Done/Failed, but leaves
// DownloadStarted and ServerStats untouched.
func TestRetry_ResetsDownloadStats(t *testing.T) {
	t.Skip("EXPECTED FAIL until B.3: RetryHistoryJob leaves DownloadStarted and ServerStats from the prior attempt")
}
