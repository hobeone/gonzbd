package queue

import (
	"context"
	"testing"
)

// artifactOrderStore wraps a real Store and, on DeleteJobArtifacts, calls
// back into q to record whether the job is still reachable through q.byID
// at that exact moment. It embeds Store so every other method delegates
// unchanged.
type artifactOrderStore struct {
	Store
	q *Queue

	called                  bool
	jobStillPresentAtDelete bool
}

func (s *artifactOrderStore) DeleteJobArtifacts(ctx context.Context, id string) error {
	s.called = true
	if _, err := s.q.Get(id); err == nil {
		// The job is still in q.byID while its on-disk artifacts are being
		// unlinked — exactly the window that lets a concurrent
		// Queue.SnapshotJob clone the job (under RLock) and then hydrate it
		// from a manifest file that has already been deleted.
		s.jobStillPresentAtDelete = true
	}
	return s.Store.DeleteJobArtifacts(ctx, id)
}

// TestQueueRemove_DeletesArtifactsAfterJobLeavesQueue pins the ORDERING fix
// for the manifest-deletion race described in issue #267: Queue.Remove must
// not unlink a job's on-disk manifest/progress files until after the job has
// already left q.byID. A test that only checks "the files are gone
// afterwards" would pass against the old, buggy order too (store.Remove
// unlinked the files before evictJobLocked/removeAtLocked/delete ran) — this
// test instead has the deletion callback observe q.byID's state at the
// instant it fires.
func TestQueueRemove_DeletesArtifactsAfterJobLeavesQueue(t *testing.T) {
	real, dir := setupResidencyTestStore(t)
	store := &artifactOrderStore{Store: real}
	q := New(WithStore(store), WithStateDir(dir))
	store.q = q

	job := makeMultiFileJob(t, "remove-order", 1, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := q.Remove(job.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if !store.called {
		t.Fatal("DeleteJobArtifacts was never called")
	}
	if store.jobStillPresentAtDelete {
		t.Error("job was still present in q.byID when DeleteJobArtifacts ran — " +
			"the artifact unlink raced ahead of the job's removal from the queue")
	}
}
