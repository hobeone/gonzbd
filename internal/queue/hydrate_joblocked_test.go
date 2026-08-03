package queue

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// describesSameJobAs is the precondition guarding every pairing of a live
// JobProgress with a manifest read from disk. Its nil cases are not
// hypothetical defensive padding: hydrateSnapshot calls it on a clone whose
// progress comes from cloneJob, and a Job constructed outside the queue has
// neither field set.
func TestDescribesSameJobAs(t *testing.T) {
	m := mustManifest(t, makeMultiFileJob(t, "dsja", 2, 3))
	p := newJobProgress(m)

	if !p.describesSameJobAs(m) {
		t.Error("a progress built from m does not match m")
	}
	if p.describesSameJobAs(nil) {
		t.Error("matched a nil manifest; there is nothing to agree with")
	}
	var nilProgress *JobProgress
	if nilProgress.describesSameJobAs(m) {
		t.Error("a nil progress matched a manifest")
	}
	if nilProgress.describesSameJobAs(nil) {
		t.Error("two absent values are not a matching pair")
	}

	// A manifest of a different shape must not match, which is the case that
	// the stale-manifest guard actually fires on.
	other := mustManifest(t, makeMultiFileJob(t, "dsja-other", 3, 3))
	if p.describesSameJobAs(other) {
		t.Error("progress sized for 2 files matched a 3-file manifest")
	}
}

// hydrateJobLocked directly, rather than only through SetStatus and Retry.
// It is the shared hydration primitive both of those layer on, and its
// no-op contract — already hydrated, or no persistence configured — is the
// part callers most rely on and least exercise.
func TestHydrateJobLocked(t *testing.T) {
	t.Run("no-op when no persistence is configured", func(t *testing.T) {
		q := New()
		job := makeMultiFileJob(t, "hjl-nopersist", 1, 2)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		q.mu.Lock()
		err := q.hydrateJobLocked(job, job.ID)
		q.mu.Unlock()
		if err != nil {
			t.Errorf("hydrateJobLocked = %v on a queue with no store or state dir, want nil", err)
		}
	})

	t.Run("no-op when already hydrated", func(t *testing.T) {
		store, dir := setupResidencyTestStore(t)
		q := New(WithStore(store), WithStateDir(dir))
		job := makeMultiFileJob(t, "hjl-resident", 1, 2)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if !manifestResident(job) {
			t.Skip("job was evicted at Add; this subtest needs a resident one")
		}
		before := job.Progress()
		q.mu.Lock()
		err := q.hydrateJobLocked(job, job.ID)
		q.mu.Unlock()
		if err != nil {
			t.Fatalf("hydrateJobLocked on a resident job = %v, want nil", err)
		}
		if job.Progress() != before {
			t.Error("hydrateJobLocked replaced the progress of an already-hydrated job")
		}
	})

	t.Run("reports an unreadable manifest and stays non-resident", func(t *testing.T) {
		store, dir := setupResidencyTestStore(t)
		q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))
		filler := makeMultiFileJob(t, "hjl-filler", 1, 1)
		if err := q.Add(filler); err != nil {
			t.Fatalf("Add filler: %v", err)
		}
		job := makeMultiFileJob(t, "hjl-corrupt", 1, 2)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if manifestResident(job) {
			t.Fatal("fixture guard: job is resident, nothing to hydrate")
		}
		if err := fsutil.WriteGzAtomicBytes(dir+"/manifests/"+job.ID+".json.gz", []byte("not gzip json")); err != nil {
			t.Fatalf("corrupt manifest: %v", err)
		}
		before := job.Progress()

		q.mu.Lock()
		err := q.hydrateJobLocked(job, job.ID)
		q.mu.Unlock()

		if err == nil {
			t.Fatal("hydrateJobLocked returned nil for an unreadable manifest")
		}
		if manifestResident(job) {
			t.Error("the job was left resident after a failed hydration")
		}
		if job.Progress() != before {
			t.Error("the pre-attempt progress was not restored; a failed hydration must not cost the job its state")
		}
		if _, mErr := job.Manifest(); errors.Is(mErr, ErrJobNotResident) {
			t.Error("Manifest() reports routine eviction after a read failure")
		}
	})
}
