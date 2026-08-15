package api

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
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

// TestBuildSlot_Par2HeldStaysTrueAfterDiscard pins that the "par2 on-demand"
// badge does not vanish at the exact moment the feature succeeds. Par2Held
// must reflect UsesOnDemandPar2 (any non-default fetch policy), not
// HasDeferredPar2 (FetchIfNeeded only) — a discarded volume is still a
// volume that was withheld from download, which is what the badge
// describes.
func TestBuildSlot_Par2HeldStaysTrueAfterDiscard(t *testing.T) {
	q, job := newOnDemandPar2Job(t)

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}

	slot := buildSlot(job, false, 0, 0, nil, app.JobCheckpointState{})
	if !slot.Par2Held {
		t.Error("Par2Held = false after DiscardDeferredPar2, want true — the badge must not disappear once the verdict comes back clean")
	}
}
