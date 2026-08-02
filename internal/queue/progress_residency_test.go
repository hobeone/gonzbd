package queue

import (
	"testing"
)

// The contract: every job in the queue has progress, whatever its status
// and whether or not its manifest is resident. This is what makes every
// reporting path total.
func TestProgressResidentAcrossEvictionAndRestart(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	active := makeMultiFileJob(t, "active", 1, 4)
	if err := q.Add(active); err != nil {
		t.Fatalf("Add active: %v", err)
	}
	queued := makeMultiFileJob(t, "queued", 1, 4)
	if err := q.Add(queued); err != nil {
		t.Fatalf("Add queued: %v", err)
	}

	// queued is beyond maxActive, so its manifest is evicted. Its progress
	// must not be.
	q.mu.RLock()
	qj := q.byID[queued.ID]
	manifestEvicted := qj.manifest == nil
	progressPresent := qj.progress != nil
	q.mu.RUnlock()

	if !manifestEvicted {
		t.Fatal("fixture guard: queued job is still resident, eviction is not being exercised")
	}
	if !progressPresent {
		t.Error("progress was dropped along with the manifest")
	}

	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q2, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	q2.mu.RLock()
	defer q2.mu.RUnlock()
	for id, j := range q2.byID {
		if j.progress == nil {
			t.Errorf("job %s came back from restart with nil progress", id)
			continue
		}
		if got := j.progress.done.Len(); got != 4 {
			t.Errorf("job %s: progress sized to %d articles, want 4 — it was "+
				"allocated without the real article count", id, got)
		}
	}
}
