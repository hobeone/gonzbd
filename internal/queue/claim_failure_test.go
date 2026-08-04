package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// These cover three helpers on the promotion/claim-failure path that had no
// direct test, surfaced by check_test_alignment when this change touched
// queue.go. They are pre-existing gaps, covered rather than exempted.

// finishClaimFailure sets a manifest aside so a job that could not be claimed
// does not get retried against the same unreadable file. It must only do that
// when it was given a path: a torn manifest/rows pair passes an empty one,
// because the file parses fine and is the artifact reconciliation works from.
func TestFinishClaimFailure_SetsAsideOnlyThePathItIsGiven(t *testing.T) {
	dir := t.TempDir()
	q := New(WithStateDir(dir))

	write := func(name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("manifest"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return p
	}

	named := write("named.json.gz")
	q.finishClaimFailure(t.Context(), claimFailure{job: &Job{ID: "a"}, manifestPath: named})
	if _, err := os.Stat(named + ".corrupt"); err != nil {
		t.Errorf("manifest was not set aside: %v", err)
	}
	if _, err := os.Stat(named); err == nil {
		t.Error("the original manifest is still in place, so promotion would retry it")
	}

	// An empty path must leave every manifest alone.
	kept := write("kept.json.gz")
	q.finishClaimFailure(t.Context(), claimFailure{job: &Job{ID: "b"}, manifestPath: ""})
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("an empty manifestPath disturbed an unrelated manifest: %v", err)
	}
	if _, err := os.Stat(kept + ".corrupt"); err == nil {
		t.Error("an empty manifestPath still produced a .corrupt file")
	}
}

// findNextQueuedCandidateLocked picks the first job eligible for promotion.
// Every one of its four conditions excludes a job that would otherwise be
// promoted twice or promoted when it should not run at all.
func TestFindNextQueuedCandidateLocked_Eligibility(t *testing.T) {
	// maxActive=1 so only the first Add stays resident; the candidate has to
	// be non-resident, which is one of the four conditions under test.
	q := New(WithStateDir(t.TempDir()), WithMaxActiveJobs(1))

	add := func(name string, status constants.Status) *Job {
		t.Helper()
		j := makeMultiFileJob(t, name, 1, 1)
		if err := q.Add(j); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
		j.Status = status
		return j
	}

	downloading := add("cand-downloading", constants.StatusDownloading)
	queued := add("cand-queued", constants.StatusQueued)

	q.mu.Lock()
	got := q.findNextQueuedCandidateLocked()
	q.mu.Unlock()
	if got == nil || got.ID != queued.ID {
		t.Fatalf("picked %v, want the queued job %s", got, queued.ID)
	}
	_ = downloading

	// PostProc, in-flight promotion, and a paused queue each exclude it.
	queued.PostProc = true
	q.mu.Lock()
	got = q.findNextQueuedCandidateLocked()
	q.mu.Unlock()
	if got != nil {
		t.Errorf("picked a PostProc job %s", got.ID)
	}
	queued.PostProc = false

	q.mu.Lock()
	q.promoting[queued.ID] = true
	got = q.findNextQueuedCandidateLocked()
	delete(q.promoting, queued.ID)
	q.mu.Unlock()
	if got != nil {
		t.Errorf("picked %s while it was already being promoted", got.ID)
	}

	q.mu.Lock()
	q.paused = true
	got = q.findNextQueuedCandidateLocked()
	q.paused = false
	q.mu.Unlock()
	if got != nil {
		t.Errorf("picked %s while the queue was paused", got.ID)
	}
}

// undeferRecoveryLocked clears the Deferred flag on the given files and
// reports whether anything changed. Indices that are out of range or not
// deferred are ignored rather than treated as an error, so a caller acting on
// a stale view cannot corrupt the set.
func TestUndeferRecoveryLocked_ClearsOnlyDeferredFilesInRange(t *testing.T) {
	q := New(WithStateDir(t.TempDir()))
	job := makeMultiFileJob(t, "undefer", 3, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	q.mu.Lock()
	job.progress.files[1].Deferred = true
	// Out of range and not-deferred indices alongside the real one.
	changed := q.undeferRecoveryLocked(job, []int{-1, 99, 0, 1})
	q.mu.Unlock()

	if !changed {
		t.Fatal("reported no change despite clearing a deferred file")
	}
	if job.progress.files[1].Deferred {
		t.Error("file 1 is still deferred")
	}
	if !job.progress.par2Recovered {
		t.Error("par2Recovered was not set, so the job does not record that recovery ran")
	}

	q.mu.Lock()
	changed = q.undeferRecoveryLocked(job, []int{0, 1, 2})
	q.mu.Unlock()
	if changed {
		t.Error("reported a change when nothing was deferred")
	}
}

// A promotion that finds the stored rows disagreeing with the stored manifest
// must not label the manifest corrupt. The file parses; it is the pair that is
// wrong, and it is the artifact reconciliation reads from.
func TestPromoteNext_StaleRowsDoNotCondemnTheManifest(t *testing.T) {
	store, dir, job := tornPair(t, "promote-stale", 3, 1)

	q := New(WithStore(store), WithStateDir(dir))
	// Load the job the way a restart would, but keep the torn pair: Get would
	// reconcile it, so attach it directly and mark it queued for promotion.
	job.Status = constants.StatusQueued
	q.mu.Lock()
	q.jobs = append(q.jobs, job)
	q.byID[job.ID] = job
	q.mu.Unlock()

	// Drop residency so PromoteNext rebuilds progress from the shrunk
	// manifest on disk and then hits the pre-discard rows.
	job.setResidency(nil, job.progress)

	q.PromoteNext(t.Context())

	manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if _, err := os.Stat(manifestPath + ".corrupt"); err == nil {
		t.Error("a manifest that parses was set aside as corrupt")
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("the manifest was moved or removed: %v", err)
	}
	if job.Warning != "" && strings.Contains(job.Warning, "Corrupt manifest") {
		t.Errorf("warning %q blames the manifest for a disagreement between it and the rows", job.Warning)
	}
}
