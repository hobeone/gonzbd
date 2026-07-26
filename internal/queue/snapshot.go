package queue

import (
	"context"
	"path/filepath"
)

func (q *Queue) ensureManifestLoadedLocked(j *Job) {
	if j.manifest != nil || (q.store == nil && q.stateDir == "") {
		return
	}
	manifestPath := filepath.Join(q.stateDir, "manifests", j.ID+".json.gz")
	var m Manifest
	if err := readGzJSON(manifestPath, &m); err == nil {
		m.buildMessageIDIndex()
		j.manifest = &m
		j.progress = newJobProgress(&m)
		if q.store != nil {
			_ = q.store.RestoreJobProgress(context.Background(), j)
		}
	}
}

// Snapshot returns a point-in-time, deep-copied view of all jobs in the
// queue. It is intended for testing and consistent-read views (e.g. for
// API responses).
//
// The returned slice and the Jobs within it are fresh allocations; mutations
// to the returned objects do not affect the Queue's internal state.
func (q *Queue) Snapshot() []*Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	res := make([]*Job, 0, len(q.jobs))
	for _, j := range q.jobs {
		q.ensureManifestLoadedLocked(j)
		res = append(res, cloneJob(j))
	}
	return res
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
	q.mu.Lock()
	defer q.mu.Unlock()

	j, ok := q.byID[id]
	if !ok {
		return nil
	}
	q.ensureManifestLoadedLocked(j)
	return cloneJob(j)
}
