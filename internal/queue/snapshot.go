package queue

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
)

// Snapshot returns a point-in-time, deep-copied view of all jobs in the
// queue. It is intended for testing and consistent-read views (e.g. for
// API responses).
//
// The returned slice and the Jobs within it are fresh allocations; mutations
// to the returned objects do not affect the Queue's internal state.
//
// Snapshot does not hydrate. A non-resident job comes back with a nil
// manifest and its promoted scalars and JobProgress intact, which is
// everything the whole-queue callers need: the API queue listing, the
// delete-all enumeration, and startup's completed-job sweep. Hydrating here
// meant a disk read per non-resident job on every poll of a listing that is
// polled continuously, and it opened the window where a job removed between
// the clone and the read yields an error for an operation that had already
// succeeded. Use SnapshotJob when one job's file-level detail is genuinely
// required; it still hydrates, once, for that job.
func (q *Queue) Snapshot() []*Job {
	q.mu.RLock()
	defer q.mu.RUnlock()
	res := make([]*Job, 0, len(q.jobs))
	for _, j := range q.jobs {
		res = append(res, cloneJob(j))
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
func hydrateSnapshot(log *slog.Logger, stateDir string, store Store, cp *Job) {
	manifestPath := filepath.Join(stateDir, "manifests", cp.ID+".json.gz")
	// cloneJob already copied the source job's progress, which is accurate
	// and always resident. Hold on to it: setResidency below replaces it
	// with an all-zero JobProgress for the store to fill in, and both
	// failure paths need to put the accurate one back rather than return
	// the placeholder.
	priorProgress := cp.progress
	var m Manifest
	if err := readGzJSON(manifestPath, &m); err != nil {
		// Record and log rather than returning a nil manifest silently. A
		// job that cannot load its manifest is not the same as one that was
		// evicted, and every consumer downstream sees only the pointer —
		// so the distinction has to travel with the job.
		cp.setHydrateFailure(priorProgress, fmt.Errorf("read manifest %s: %w", manifestPath, err))
		log.Error("snapshot: manifest unreadable, job cannot be hydrated",
			"job_id", cp.ID, "path", manifestPath, "err", err)
		return
	}
	m.buildMessageIDIndex()
	// Attach the manifest to the progress the clone already carries; do not
	// build a fresh one.
	//
	// Replacing it with newJobProgress was correct while a non-resident job
	// had nil progress — there was nothing to discard. #276 made JobProgress
	// permanently resident, so cloneJob now hands this an accurate deep copy
	// and rebuilding threw it away (#287). Deferred was the field that made
	// it visible: on-demand par2 holds recovery volumes back in progress
	// alone, and every snapshot reported them as not deferred, so
	// maybeReleaseRecoveryVolumes saw nothing to release. The line did not
	// change; its precondition did.
	//
	// RestoreJobProgress below still runs, and still matters: SQLiteStore.Get
	// fills progress from the store only for resident statuses, so a job
	// restored at boot in StatusQueued/StatusPaused arrives here with progress
	// that has never seen the stored counters. It sets the completion and
	// deferral flags and marks articles done without clearing anything, and
	// assigns the three per-file byte counters outright; since this only runs
	// for a non-resident job, which by definition is not downloading, those
	// counters are not moving underneath it.
	//
	// The pair has to describe the same job, though. The manifest just read
	// from disk can be older than the progress in memory — DiscardDeferredPar2
	// shrinks both in memory and never rewrites the file — and handing a
	// mismatched pair to RestoreJobProgress panics inside recompute, on a
	// background goroutine with no recover. Report it instead: a manifest that
	// disagrees with the job's own progress is not this job's manifest, and
	// serving it would mean reporting files the job no longer has.
	if !priorProgress.describesSameJobAs(&m) {
		cp.setHydrateFailure(priorProgress, fmt.Errorf(
			"%w: stored manifest for job %s describes %d files/%d articles but its progress holds %d/%d",
			ErrManifestStale, cp.ID, m.NumFiles(), m.NumArticles(),
			len(priorProgress.files), priorProgress.done.Len()))
		log.Error("snapshot: stored manifest no longer matches the job's progress, refusing to pair them",
			"job_id", cp.ID, "path", manifestPath,
			"manifest_files", m.NumFiles(), "progress_files", len(priorProgress.files))
		return
	}
	cp.setResidency(&m, priorProgress)
	// cp came from cloneJob with cp.manifest == nil, meaning the source
	// Job's scalars were never populated (the restore path that produced
	// it — e.g. SQLiteStore.Get for a StatusQueued/StatusPaused job —
	// only loads a manifest for resident statuses). Backfill them from
	// the manifest this call just read, the same way hydrateJobLocked
	// does for its own non-resident restore case: no extra I/O, the read
	// already happened above.
	cp.setScalarsFromManifest(&m)
	if store != nil {
		if err := store.RestoreJobProgress(context.Background(), cp); err != nil {
			// setResidency above has already swapped in the all-zero
			// JobProgress this call was meant to fill from the stored
			// counters. Returning it would present a part-downloaded job as
			// having downloaded nothing — to the par2 recovery check, the
			// pipeline's registerFile, DirectUnpack and the API per-file
			// listing at once, none of which can tell a zeroed progress from
			// a genuine one. Fail closed exactly as hydrateJobLocked does on
			// this same call, restoring the accurate progress; the scalars
			// stay, since the manifest they came from was read successfully.
			cp.setHydrateFailure(priorProgress, fmt.Errorf("restore progress for job %s: %w", cp.ID, err))
			log.Error("snapshot: progress restore failed, job cannot be hydrated",
				"job_id", cp.ID, "err", err)
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
		ID:        j.ID,
		Filename:  j.Filename,
		NZBBackup: j.NZBBackup,
		Name:      j.Name,
		Password:  j.Password,
		URL:       j.URL,
		Category:  j.Category,
		Priority:  j.Priority,
		Status:    j.Status,
		PP:        j.PP,
		Script:    j.Script,
		Added:     j.Added,
		MD5:       j.MD5,
		AvgAge:    j.AvgAge,
		Warning:   j.Warning,
		PostProc:  j.PostProc,

		// Manifest-derived scalars are immutable once set and do not depend
		// on residency — they must be carried verbatim so a snapshot's
		// TotalBytes/NumFiles/etc. never requires hydration.
		totalBytes:  j.totalBytes,
		numFiles:    j.numFiles,
		numArticles: j.numArticles,
		par2Bytes:   j.par2Bytes,
		par2Files:   j.par2Files,
	}

	// Read all three residency fields under one lock. hydrateErr is carried
	// so a clone of a job whose hydration failed still reports why, rather
	// than reverting to looking merely evicted; the manifest and progress
	// pointers are taken directly rather than through Manifest()/Progress()
	// both because a nil manifest here is simply "not resident" (which
	// cloneJob copies verbatim) and because those getters take the lock
	// separately. setResidency swaps the manifest/progress pair atomically
	// precisely so no reader sees one updated and the other stale; taking
	// two locks here would reintroduce that window, pairing a pre-swap
	// manifest with a post-swap progress. The clone itself happens after the
	// unlock: it reads JobProgress *contents*, which residencyMu does not
	// guard (q.mu does — see Progress's doc comment), so holding the
	// residency lock across it would widen the critical section for nothing.
	j.residencyMu.RLock()
	cp.hydrateErr = j.hydrateErr
	cp.manifest = j.manifest
	progress := j.progress
	j.residencyMu.RUnlock()
	if progress != nil {
		cp.progress = progress.clone()
	}

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
	log := q.log
	q.mu.RUnlock()

	if cp.manifest == nil && (store != nil || stateDir != "") {
		hydrateSnapshot(log, stateDir, store, cp)
	}
	return cp
}
