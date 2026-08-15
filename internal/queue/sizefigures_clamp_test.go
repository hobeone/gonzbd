package queue

import (
	"testing"
)

// TestSizeFigures_ClampsRemainingPerFileNotPerJob restores the only pin on the
// per-file clamp, which this branch's test deletions removed.
//
// Verified by mutation before being written: deleting the `left > 0` guard
// outright left `./internal/queue` and `./internal/api` entirely green.
//
// The clamp is per FILE, not per job, and the difference is the whole point.
// The deleted Store.RemainingBytesByJob summed `bytes - bytes_downloaded`
// across a job and clamped the total at zero, so one over-downloaded file ate
// into another file's real shortfall. sizeFigures clamps each file
// independently: a file whose downloaded+failed exceeds its declared bytes
// contributes zero rather than a negative, and cannot mask a sibling's genuine
// remainder.
//
// Over-download is ordinary rather than exotic. FileProgress.Bytes is the
// NZB's declared per-file total, an advisory figure a server is free to
// contradict, and BytesDownloaded sums what actually arrived — so the two
// disagreeing by a few bytes is a property of the wire, not a defect.
//
// Driven through sizeFigures rather than through one residency's constructor,
// because both residencies now derive their figures from this one walk: a pin
// on newJobProgressSized alone would say nothing about the resident path.
func TestSizeFigures_ClampsRemainingPerFileNotPerJob(t *testing.T) {
	p := &JobProgress{files: []FileProgress{
		// Over-downloaded by 50. Must contribute 0, never -50.
		{Bytes: 100_000, BytesDownloaded: 100_050, Fetch: FetchAlways},
		// Genuinely 30 bytes short.
		{Bytes: 100_000, BytesDownloaded: 99_970, Fetch: FetchAlways},
	}}

	expected, remaining := p.sizeFigures()

	if remaining != 30 {
		t.Errorf("remaining = %d, want 30 — file 0's over-download must clamp to 0 on its "+
			"own, not net against file 1's real 30-byte shortfall. Without the clamp this "+
			"reports -20, and RemainingBytes goes backwards as a job nears completion",
			remaining)
	}
	if expected != 200_000 {
		t.Errorf("expected = %d, want 200_000 — the clamp governs the REMAINING figure "+
			"only; a file's declared size still counts toward what the job is fetching",
			expected)
	}
}

// TestSizeFigures_ClampsFailedBytesToo covers the other term the clamp guards.
//
// A file can be over-accounted from either direction: bytes that arrived, or
// bytes charged as permanently failed. The subtraction is
// `Bytes - BytesDownloaded - FailedBytes`, so a job whose failed bytes exceed a
// file's declared size drives the same negative, and the review that surfaced
// this arrived through exactly that arithmetic.
func TestSizeFigures_ClampsFailedBytesToo(t *testing.T) {
	p := &JobProgress{files: []FileProgress{
		{Bytes: 1000, BytesDownloaded: 400, FailedBytes: 700, Fetch: FetchAlways},
		{Bytes: 1000, BytesDownloaded: 250, Fetch: FetchAlways},
	}}

	_, remaining := p.sizeFigures()

	if remaining != 750 {
		t.Errorf("remaining = %d, want 750 — file 0 is over-accounted by 100 and must "+
			"contribute 0, leaving only file 1's genuine 750", remaining)
	}
}

// TestSizeFigures_ACompleteFileContributesNoRemainder pins the other early
// return in the same walk, so the clamp test above cannot be satisfied by a
// guard that happens to swallow both cases.
//
// Complete means the assembler is finished with the file, which is NOT the same
// as every article having arrived: a permanently failed article closes the file
// with a gap. So a complete file can still show a shortfall by subtraction, and
// counting it would report work outstanding that nothing will ever do.
func TestSizeFigures_ACompleteFileContributesNoRemainder(t *testing.T) {
	p := &JobProgress{files: []FileProgress{
		{Bytes: 1000, BytesDownloaded: 600, Complete: true, Fetch: FetchAlways},
		{Bytes: 1000, BytesDownloaded: 250, Fetch: FetchAlways},
	}}

	expected, remaining := p.sizeFigures()

	if remaining != 750 {
		t.Errorf("remaining = %d, want 750 — a file the assembler has finished with has "+
			"nothing outstanding, even where its bytes fall short of the declared size "+
			"because an article failed permanently", remaining)
	}
	if expected != 2000 {
		t.Errorf("expected = %d, want 2000", expected)
	}
}
