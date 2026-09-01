package app

import "testing"

func TestDownloadCompleteness(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		totalBytes  int64
		failedBytes int64
		want        int64
	}{
		{name: "no failures is fully complete", totalBytes: 1000, failedBytes: 0, want: 100},
		{name: "partial failure is below 100", totalBytes: 1000, failedBytes: 34, want: 96},
		{name: "half failed", totalBytes: 1000, failedBytes: 500, want: 50},
		{name: "all bytes failed", totalBytes: 1000, failedBytes: 1000, want: 0},
		{name: "more failed than total clamps to zero", totalBytes: 1000, failedBytes: 2000, want: 0},
		{name: "zero total returns zero", totalBytes: 0, failedBytes: 0, want: 0},
		{name: "negative total returns zero", totalBytes: -10, failedBytes: 0, want: 0},
		{name: "negative failed treated as zero", totalBytes: 1000, failedBytes: -5, want: 100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := downloadCompleteness(tc.totalBytes, tc.failedBytes)
			if got != tc.want {
				t.Errorf("downloadCompleteness(%d, %d) = %d, want %d",
					tc.totalBytes, tc.failedBytes, got, tc.want)
			}
		})
	}
}

// TestDownloadCompletenessFailedBytesNotComplete is the explicit regression
// guard for the bug where a job with permanently-failed articles reported
// 100% "Download health". A failed article is marked both Done and Failed,
// so the previous article-count ratio (done/total) ignored failures and
// always read ~100% on a finished job. Any non-zero failed bytes must now
// produce a completeness strictly below 100.
func TestDownloadCompletenessFailedBytesNotComplete(t *testing.T) {
	t.Parallel()
	// ~3.4% of bytes failed (the originally-reported "9.9 MiB failed").
	const total int64 = 291 * 1024 * 1024
	const failed int64 = 10380902 // 9.9 MiB

	got := downloadCompleteness(total, failed)
	if got >= 100 {
		t.Fatalf("completeness with %d/%d failed bytes = %d, want < 100", failed, total, got)
	}
	if got != 96 {
		t.Errorf("completeness = %d, want 96 (≈96.6%% truncated)", got)
	}
}
