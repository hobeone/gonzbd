package queue

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// Save persists the queue through its Store and records dir as the state
// directory (where manifests live).
//
// A Queue with no Store has no persistence at all, and Save is a no-op for
// it beyond recording dir. That is the configuration's defined meaning, not
// a silently skipped write: New() without WithStore builds an in-memory
// queue, the same condition hydrateJobLocked and Add already treat as "no
// persistence configured". Every production entry point supplies a Store
// (see Load).
func (q *Queue) Save(dir string) error {
	q.dirty.Store(false)

	if q.store != nil {
		if err := q.saveStore(dir); err != nil {
			q.dirty.Store(true)
			return err
		}
	}
	q.mu.Lock()
	q.stateDir = dir
	q.mu.Unlock()
	return nil
}

func (q *Queue) saveStore(_ string) error {
	q.mu.RLock()
	snapshots := make([]*Job, 0, len(q.jobs))
	for _, job := range q.jobs {
		snapshots = append(snapshots, cloneJob(job))
	}
	paused := q.paused
	q.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Persist the jobs before the paused flag: the jobs are the data worth
	// saving, so a failure to write one bool must not cost us the whole
	// queue. Both are attempted regardless, and their errors joined, rather
	// than letting the first failure short-circuit the second write.
	jobsErr := q.store.UpdateBatch(ctx, snapshots)
	if jobsErr != nil {
		jobsErr = fmt.Errorf("queue: save jobs: %w", jobsErr)
	}
	pausedErr := q.store.SetPaused(ctx, paused)
	if pausedErr != nil {
		pausedErr = fmt.Errorf("queue: save paused state: %w", pausedErr)
	}
	if err := errors.Join(jobsErr, pausedErr); err != nil {
		// Skip Prune: it deletes rows absent from the live set. Either
		// write failing means this cycle's store state is not what we
		// intended, so the guard is deliberately broader than "the live
		// set is not durable" — after a paused-only failure the jobs did
		// land, and Prune would be safe by that narrower reading alone.
		//
		// The breadth is the point. Deferring Prune costs one cycle of
		// stale rows and the next checkpoint runs it; running a
		// destructive delete against a store that just failed a write
		// costs rows. A SetPaused failure is also evidence about the
		// store rather than about that one bool — a locked database or a
		// full disk fails it the same way — which is not a state to start
		// deleting in.
		return err
	}
	if err := q.store.Prune(ctx); err != nil {
		return fmt.Errorf("queue: prune store: %w", err)
	}
	return nil
}

// newJobProgressSized builds a JobProgress sized to files (one element per
// file) without requiring a resident Manifest — see Store.ArticleCountsByJob.
// Used by Load to give a non-resident job (StatusQueued/StatusPaused at
// restart) a real JobProgress instead of leaving it nil.
//
// Every article starts undone/unfailed/unemitted: this sizes progress for
// reporting, it does not restore true per-article state, which needs the
// manifest. That restoration already happens whenever the job is promoted
// back to resident.
//
// Per-file Bytes/Complete/Fetch are carried so RemainingBytes (see
// derivedRemainingBytes) has the file sizes it needs at any residency,
// including a job that restarted non-resident. They replace the pre-summed
// figure this used to take from Store.RemainingBytesByJob.
//
// BytesDownloaded and FailedBytes are seeded from FileMeta, and that is what
// makes a non-resident job report the same figures a resident one does. They
// arrive from two different places because they have two different
// authorities — file_extents.bytes_durable for the first, job_files.failed_bytes
// for the second — which FileMeta's own doc explains.
//
// Seeding them here does NOT recreate the two-writer defect that removed the
// old columns (#306, #337). Nothing maintains these fields in parallel with
// the facts: promotion calls Store.RestoreJobProgress, which ends in
// JobProgress.recompute, and recompute ASSIGNS BytesDownloaded, FailedBytes and
// the job-level failedBytes from the manifest and the article bitmaps rather
// than adding to them. So the seed is superseded wholesale the moment real
// per-article state exists, which is S4 — the recomputation is correct by
// definition — rather than a second copy kept in step by hand. That assignment
// is also what keeps TestFailedBytes_NotDoubledByHydration true: a seed and a
// replay cannot stack.
//
// The per-article bitmaps genuinely do start clear here — every article
// starts undone/unfailed/unemitted, since restoring true per-article state
// needs the manifest and only happens once the job is promoted back to
// resident. That is why the byte figures have to be seeded at all: with no
// bits set there is nothing for a derivation to read.
func newJobProgressSized(files []FileMeta) *JobProgress {
	total := 0
	for _, f := range files {
		total += f.ArticleCount
	}
	p := &JobProgress{
		done:            newBitset(total),
		failed:          newBitset(total),
		emitted:         newBitset(total),
		files:           make([]FileProgress, len(files)),
		pendingArticles: total,
	}
	var failedTotal int64
	for fi, f := range files {
		p.files[fi].Pending = f.ArticleCount
		p.files[fi].Complete = f.Complete
		p.files[fi].Fetch = f.Fetch
		p.files[fi].IsPar2 = f.IsPar2
		p.files[fi].Bytes = f.Bytes
		p.files[fi].BytesDownloaded = f.BytesDurable
		p.files[fi].FailedBytes = f.FailedBytes
		// Summed over every file including deferred ones, matching what
		// recompute does, so the job-level figure agrees with the per-file
		// ones at both residencies. ExpectedBytes' identity
		// (downloaded = expected - failed - remaining) depends on the two
		// sides using the same exclusion set.
		failedTotal += f.FailedBytes
	}
	p.failedBytes = failedTotal
	return p
}

// Load reconstructs a Queue from dir, reading its jobs through the Store
// supplied via WithStore and their manifests from dir/manifests.
//
// A Queue built without a Store loads nothing: there is nowhere to load it
// from. Until #266 this fell through to a second, whole-queue gzip-JSON
// engine that no production path could reach — both entry points always
// construct a Store — and that carried two live defects of its own. It has
// been removed rather than repaired.
func Load(dir string, opts ...Option) (*Queue, error) {
	q := New(opts...)
	if q.store != nil {
		q.stateDir = dir
		jobs, err := q.store.List(context.Background())
		if err != nil {
			return nil, fmt.Errorf("queue: load store: %w", err)
		}
		// Fail closed, matching the two queries below and the reasoning in
		// their comment. Discarding this error silently reset the queue-wide
		// pause flag to false on startup, so a store hiccup could resume a
		// queue the operator had deliberately paused. A fresh database is not
		// an error case: IsPaused returns (false, nil) when the row is absent.
		paused, err := q.store.IsPaused(context.Background())
		if err != nil {
			return nil, fmt.Errorf("queue: load paused state: %w", err)
		}
		// Size JobProgress for every job store.Get left non-resident
		// (progress == nil): Get only restores progress for a
		// resident-status job whose manifest file is present on disk, so
		// every StatusQueued/StatusPaused job comes back from List()
		// without one.
		//
		// A resident-status job whose manifest is MISSING is not in that
		// set, and the difference matters to anyone trying to build a
		// fixture for it: when the job has files, Get calls s.Remove and
		// returns an error, and List drops it entirely rather than
		// yielding it progress-less. Only the file-less case survives Get,
		// and it reaches this loop by the StatusQueued/StatusPaused route
		// anyway. Do not write a test that expects a manifest-less resident
		// job with files to appear here — it cannot.
		// The invariant this task establishes is job.progress != nil for
		// every job in q.byID (docs/queue-lifecycle.md), so this must run
		// for all of them, not just the subset the loop below re-hydrates.
		//
		// Queried before q.mu.Lock(): Store calls are treated as I/O by
		// scripts/check_lock_io, and must not run inside the critical
		// section below. One grouped query for every job rather than one
		// query per job, so a large queued backlog costs one round trip,
		// not N.
		countsByJob, err := q.store.ArticleCountsByJob(context.Background())
		if err != nil {
			return nil, fmt.Errorf("queue: load article counts: %w", err)
		}
		for _, job := range jobs {
			if job.progress != nil {
				continue
			}
			job.progress = newJobProgressSized(countsByJob[job.ID])
		}
		func() {
			q.mu.Lock()
			defer q.mu.Unlock()
			for _, job := range jobs {
				q.jobs = append(q.jobs, job)
				q.byID[job.ID] = job
				// A resident-status job arrives from the store already
				// hydrated, or not at all: SQLiteStore.Get reads
				// manifests/<id>.json.gz whenever that file exists, from the
				// same directory this function was handed. There used to be a
				// re-read fallback here for the manifest == nil case, but it
				// could never do anything — manifest == nil means Get found
				// no file, so re-reading the identical path fails too. It was
				// removed with the rest of the unreachable persistence code
				// in #266.
				if job.manifest != nil {
					job.manifest.buildMessageIDIndex()
				}
			}
			q.paused = paused
		}()
		_ = q.store.Prune(context.Background())
		return q, nil
	}

	// No Store: nothing to load. Returning the empty queue is the whole
	// behaviour, not a fallback — see this function's doc comment.
	q.stateDir = dir
	return q, nil
}

// writeGzJSON encodes v as gzipped JSON and atomically publishes it at path.
func writeGzJSON(path string, v any) error {
	return fsutil.WriteGzAtomic(path, func(gz io.Writer) error {
		enc := json.NewEncoder(gz)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("encode: %w", err)
		}
		return nil
	})
}

// readGzJSON opens path, gunzips, and decodes JSON into v. Returns
// os.ErrNotExist (wrapped) when the file is missing so callers can
// distinguish "never persisted" from real I/O errors.
func readGzJSON(path string, v any) error {
	f, err := os.Open(path) //nolint:gosec // path built from operator-configured admin dir
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only handle

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close() //nolint:errcheck // read-only handle

	data, err := io.ReadAll(gz)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
