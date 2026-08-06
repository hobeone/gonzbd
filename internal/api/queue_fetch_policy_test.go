package api

import (
	"testing"
)

// TestFileState_DistinguishesHeldFromSkipped pins the two reasons a file is
// not being fetched. "held" is awaiting the CRC verdict; "skipped" is the
// verdict having come back clean, which is the on-demand par2 saving made
// visible per file rather than only in the history summary.
func TestFileState_DistinguishesHeldFromSkipped(t *testing.T) {
	q, job := newOnDemandPar2Job(t)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	p := job.Progress()

	// File 1 is the recovery volume, held pending the verdict.
	if got := fileState(m, p, 1); got != "held" {
		t.Errorf("held volume state = %q, want %q", got, "held")
	}
	if got := fileState(m, p, 0); got == "held" || got == "skipped" {
		t.Errorf("content file state = %q, want a fetched state", got)
	}

	// The verdict comes back clean.
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	if got := fileState(m, p, 1); got != "skipped" {
		t.Errorf("discarded volume state = %q, want %q", got, "skipped")
	}
}
