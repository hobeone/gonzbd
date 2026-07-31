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

// hydrateSnapshot loads cp's manifest/progress from disk. cp is always a
// freshly cloneJob-allocated Job that has not yet been returned to any
// caller and is never inserted into q.jobs/q.byID, so it has no concurrent
// readers at this point — a raw field-level residency lock via
// setResidency isn't required for correctness here. It's used anyway so
// there is exactly one way to assign the manifest/progress pair in this
// package, rather than two.
func hydrateSnapshot(stateDir string, store Store, cp *Job) {
	manifestPath := filepath.Join(stateDir, "manifests", cp.ID+".json.gz")
	var m Manifest
	if err := readGzJSON(manifestPath, &m); err == nil {
		m.buildMessageIDIndex()
		cp.setResidency(&m, newJobProgress(&m))
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
//
// This copies j's exported fields one at a time rather than `cp := *j`
// deliberately: Job now embeds a sync.RWMutex value (residencyMu), and a
// whole-struct copy would copy that mutex's state too — a go vet copylocks
// violation, and semantically wrong besides, since cp needs its own,
// independent, unlocked mutex rather than a snapshot of j's. Declaring cp
// as a struct literal leaves residencyMu at its zero value, which is
// exactly right: ready to use, uncontended, and unrelated to j's.
func cloneJob(j *Job) *Job {
	cp := &Job{
		ID:       j.ID,
		Filename: j.Filename,
		Name:     j.Name,
		Password: j.Password,
		URL:      j.URL,
		Category: j.Category,
		Priority: j.Priority,
		Status:   j.Status,
		PP:       j.PP,
		Script:   j.Script,
		Added:    j.Added,
		MD5:      j.MD5,
		AvgAge:   j.AvgAge,
		Warning:  j.Warning,
		PostProc: j.PostProc,
	}

	manifest := j.Manifest()
	cp.manifest = manifest
	if progress := j.Progress(); progress != nil {
		cp.progress = progress.clone()
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

	return cp
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
