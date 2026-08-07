package postproc

import (
	"fmt"
	"strings"
	"testing"
)

// TestBuildDownloadFileList_FailurePercentSharesHistoryDenominator pins #326:
// the stage log's failure percentage and the history record's completeness
// figure must divide by the same quantity.
//
// The history record derives completeness from JobProgress.ExpectedBytes() —
// the bytes the job set out to fetch, which excludes recovery volumes it has
// decided not to download. The stage log divided by Manifest.TotalBytes(),
// which counts them. For a job with both held-back volumes and real failures
// the two disagreed, and the stage log's figure was the smaller one because
// its denominator included bytes the job never intended to fetch.
//
// Reaching that state needs care. On an on-demand-par2 job a permanent
// article failure normally *releases* the deferred volumes — repair is now
// known to be needed — which would leave nothing held back. The state where
// held volumes and failures coexist is the one this test builds: the CRC
// oracle rules the volumes unnecessary (FetchNever), and only afterwards does
// an article fail permanently. undeferRecoveryLocked skips anything that is
// not FetchIfNeeded, so the volumes stay held and the denominators diverge.
func TestBuildDownloadFileList_FailurePercentSharesHistoryDenominator(t *testing.T) {
	dir := t.TempDir()

	// 200 B of content in two articles, plus a 500 B recovery volume.
	q, qjob := buildQueueJob(t, true, []fileSpec{
		{subject: "release.rar", articles: []artSpec{
			{bytes: 100, done: true},
			{bytes: 100}, // left pending; failed below, after the discard
		}},
		{subject: "x.vol000+01.par2", bytes: 500},
	})

	// The oracle rules the volume unnecessary.
	if err := q.DiscardDeferredPar2(qjob.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	// Only now does an article fail permanently. The volume is FetchNever, so
	// it is not released and stays out of the expected set.
	if _, err := q.MarkArticlesFailed(qjob.ID, []string{"f0a1@t"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}

	p := qjob.Progress()
	m, err := qjob.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	expected, total, failed := p.ExpectedBytes(), m.TotalBytes(), p.FailedBytes()
	if failed == 0 {
		t.Fatal("fixture failed to produce failed bytes; the rest of this test would be vacuous")
	}
	if expected == total {
		t.Fatalf("fixture failed to hold back any bytes (expected=%d total=%d); "+
			"the two denominators cannot diverge, so this test would pass vacuously", expected, total)
	}

	// The figure the history record reports for this same job.
	wantPct := float64(failed) / float64(expected) * 100

	job := &Job{DownloadDir: dir, Queue: qjob}
	got := strings.Join(buildDownloadFileList(job), "\n")

	want := fmt.Sprintf("%.1f%%", wantPct)
	if !strings.Contains(got, want) {
		// Show what the manifest-total denominator would have produced, so a
		// failure reads as "wrong denominator" rather than "wrong number".
		stale := fmt.Sprintf("%.1f%%", float64(failed)/float64(total)*100)
		t.Errorf("stage log should report %s (failed %d / expected %d, the denominator "+
			"the history record uses); manifest-total denominator would give %s.\nGot:\n%s",
			want, failed, expected, stale, got)
	}
}
