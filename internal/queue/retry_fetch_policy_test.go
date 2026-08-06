package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestResetForRetry_DowngradesDiscardedToHeld pins the retry rule. The clean
// verdict was computed against one download's damage profile, and a retry
// re-fetches the articles that failed, so the contents the oracle certified
// may differ — the verdict is re-derived rather than inherited.
//
// It must be a downgrade, not a reset: FetchAlways here would re-download
// every recovery volume, which is #323.
func TestResetForRetry_DowngradesDiscardedToHeld(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
		{Subject: "a.vol001+02.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v2", Bytes: 800}}},
	})
	job := &Job{ID: "retry-downgrade", Status: constants.StatusFailed}
	job.setResidency(m, newJobProgress(m))
	job.progress.files[1].Fetch = FetchNever
	job.progress.files[2].Fetch = FetchIfNeeded

	job.ResetForRetry()

	if got := job.Progress().FileFetchPolicy(1); got != FetchIfNeeded {
		t.Errorf("discarded volume after retry = %d, want FetchIfNeeded (FetchAlways would re-download it, which is #323)", got)
	}
	if got := job.Progress().FileFetchPolicy(2); got != FetchIfNeeded {
		t.Errorf("held volume after retry = %d, want FetchIfNeeded (unchanged)", got)
	}
	if got := job.Progress().FileFetchPolicy(0); got != FetchAlways {
		t.Errorf("content file after retry = %d, want FetchAlways", got)
	}
}
