package queue

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
)

// Snapshot returns a point-in-time, deep-copied view of all jobs in the
// queue. It is intended for testing and consistent-read views (e.g. for
// API responses).
//
// The returned slice and the Jobs within it are fresh allocations; mutations
// to the returned objects do not affect the Queue's internal state.
func (q *Queue) Snapshot() ([]*Job, error) {
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

	// Hydrate non-resident snapshot copies outside the lock. Fails closed
	// rather than returning the jobs that did load: a partial list is
	// indistinguishable from a complete one to a caller that ignores the
	// error, and the job that failed is precisely the one an operator needs
	// to see in order to act on it.
	for _, cp := range toHydrate {
		if err := hydrateSnapshot(stateDir, store, cp); err != nil {
			return nil, fmt.Errorf("queue: snapshot: %w", err)
		}
	}
	return res, nil
}

// hydrateSnapshot loads cp's manifest/progress from disk. cp is always a
// freshly cloneJob-allocated Job that has not yet been returned to any
// caller and is never inserted into q.jobs/q.byID, so it has no concurrent
// readers at this point — a raw field-level residency lock via
// setResidency isn't required for correctness here. It's used anyway so
// there is exactly one way to assign the manifest/progress pair in this
// package, rather than two.
//
// Returns an error rather than leaving cp non-resident on failure. Both
// fields staying nil is indistinguishable from a job that legitimately has no
// manifest, so a silent failure here reaches the caller as empty state — zero
// bytes, no files — and reads as fact. A job reachable in the queue always has
// its manifest on disk now that artifact deletion happens after the job leaves
// byID, so reaching either error means something outside the queue's control
// has damaged it.
func hydrateSnapshot(stateDir string, store Store, cp *Job) error {
	manifestPath := filepath.Join(stateDir, "manifests", cp.ID+".json.gz")
	var m Manifest
	if err := readGzJSON(manifestPath, &m); err != nil {
		return fmt.Errorf("read manifest for job %s: %w", cp.ID, err)
	}
	m.buildMessageIDIndex()
	cp.setResidency(&m, newJobProgress(&m))
	if store != nil {
		// Treated as fatal for the same reason Queue.hydrateJobLocked treats
		// it as fatal: newJobProgress built an all-zero JobProgress for this
		// call to fill in, so continuing past a failure hands back a
		// part-downloaded job that reports no progress at all.
		if err := store.RestoreJobProgress(context.Background(), cp); err != nil {
			cp.setResidency(nil, nil)
			return fmt.Errorf("restore progress for job %s: %w", cp.ID, err)
		}
	}
	return nil
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
	// cp.lastKnownRemainingBytes is deliberately left at its zero value,
	// not copied from j. A clone with cp.manifest == nil already gets its
	// manifest/progress independently reconstructed from disk by
	// hydrateSnapshot (see Snapshot/SnapshotJob below) whenever a state
	// directory or store is configured, which is a live re-read rather
	// than a cached approximation. Outside that path (e.g. saveStore's use
	// of cloneJob) the clone is only ever serialized via job_files/store
	// rows, which don't have a column for this field, so there is nothing
	// for a stale copy to serve.
	cp.lastKnownRemainingBytes = 0

	// Deep copy maps. maps.Clone would be shallow — the value slices would
	// stay shared with j — so the values are cloned individually.
	if j.Meta != nil {
		cp.Meta = make(map[string][]string, len(j.Meta))
		for k, v := range j.Meta {
			cp.Meta[k] = slices.Clone(v)
		}
	}

	// slices.Clone preserves nil, so this needs no nil guard.
	cp.Groups = slices.Clone(j.Groups)

	return cp
}

// SnapshotJob returns a point-in-time, deep-copied view of a single job by
// ID. Reports ErrNotFound when no such job exists, and a distinct error when
// the job exists but its stored state could not be loaded.
func (q *Queue) SnapshotJob(id string) (*Job, error) {
	q.mu.RLock()
	j, ok := q.byID[id]
	if !ok {
		q.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	cp := cloneJob(j)
	stateDir := q.stateDir
	store := q.store
	q.mu.RUnlock()

	if cp.manifest == nil && (store != nil || stateDir != "") {
		// Deliberately not ErrNotFound: the job exists, its stored state is
		// damaged. Callers distinguish the two — a missing job is a 404, a
		// corrupt one is not, and collapsing them would report corruption as
		// routine absence.
		if err := hydrateSnapshot(stateDir, store, cp); err != nil {
			return nil, fmt.Errorf("queue: snapshot job %s: %w", id, err)
		}
	}
	return cp, nil
}
