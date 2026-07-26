package queue

import (
	"context"
	"path/filepath"
)

// Snapshot returns a point-in-time, deep-copied view of all jobs in the
// queue. It is intended for testing and consistent-read views (e.g. for
// API responses).
//
// The returned slice and the Jobs within it are fresh allocations; mutations
// to the returned objects do not affect the Queue's internal state.
func (q *Queue) Snapshot() []*Job {
	q.mu.RLock()
	res := make([]*Job, 0, len(q.jobs))
	var toHydrate []*Job
	for _, j := range q.jobs {
		cp := cloneJob(j)
		res = append(res, cp)
		if cp.manifest == nil && (q.store != nil || q.stateDir != "") {
			toHydrate = append(toHydrate, cp)
		}
	}
	stateDir := q.stateDir
	store := q.store
	q.mu.RUnlock()

	// Hydrate non-resident snapshot copies outside the lock
	for _, cp := range toHydrate {
		hydrateSnapshot(stateDir, store, cp)
	}
	return res
}

func hydrateSnapshot(stateDir string, store Store, cp *Job) {
	manifestPath := filepath.Join(stateDir, "manifests", cp.ID+".json.gz")
	var m Manifest
	if err := readGzJSON(manifestPath, &m); err == nil {
		m.buildMessageIDIndex()
		cp.manifest = &m
		cp.progress = newJobProgress(&m)
		if store != nil {
			_ = store.RestoreJobProgress(context.Background(), cp)
		}
	}
}

// cloneJob shares manifest by reference (it is immutable after
// construction — see Manifest's own doc comment) and deep-copies progress
// via JobProgress.clone(). Getting this backwards would either let
// snapshots diverge from the live manifest (a correctness bug, since
// Manifest is meant to be immutable) or pay a full manifest deep-copy on
// every snapshot for no reason (a performance regression).
func cloneJob(j *Job) *Job {
	cp := *j

	cp.manifest = j.manifest
	if j.progress != nil {
		cp.progress = j.progress.clone()
	} else {
		cp.progress = nil
	}

	// Deep copy maps
	if j.Meta != nil {
		cp.Meta = make(map[string][]string, len(j.Meta))
		for k, v := range j.Meta {
			vCp := make([]string, len(v))
			copy(vCp, v)
			cp.Meta[k] = vCp
		}
	}

	// Deep copy slices
	if j.Groups != nil {
		cp.Groups = make([]string, len(j.Groups))
		copy(cp.Groups, j.Groups)
	}

	return &cp
}

// SnapshotJob returns a point-in-time, deep-copied view of a single job
// by ID. Returns nil if the job is not found.
func (q *Queue) SnapshotJob(id string) *Job {
	q.mu.RLock()
	j, ok := q.byID[id]
	if !ok {
		q.mu.RUnlock()
		return nil
	}
	cp := cloneJob(j)
	stateDir := q.stateDir
	store := q.store
	q.mu.RUnlock()

	if cp.manifest == nil && (store != nil || stateDir != "") {
		hydrateSnapshot(stateDir, store, cp)
	}
	return cp
}
