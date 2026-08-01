package queue

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// persistenceVersion identifies the on-disk format. Bump when a
// backwards-incompatible change lands; Load refuses unknown versions
// rather than silently misinterpreting them.
const persistenceVersion = 1

// indexFile is the top-level queue.json.gz document. It holds only
// the information needed to order jobs on reload plus the queue-wide
// pause flag; per-job state lives in jobs/<id>.json.gz.
type indexFile struct {
	Version int      `json:"version"`
	JobIDs  []string `json:"job_ids"`
	Paused  bool     `json:"paused,omitempty"`
}

// Save serialises the queue to dir.
func (q *Queue) Save(dir string) error {
	q.dirty.Store(false)

	if q.store != nil {
		if err := q.saveStore(dir); err != nil {
			q.dirty.Store(true)
			return err
		}
		q.mu.Lock()
		q.stateDir = dir
		q.mu.Unlock()
		return nil
	}

	if err := q.saveInner(dir); err != nil {
		q.dirty.Store(true)
		return err
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
		// Skip Prune: it deletes rows absent from the live set, which is
		// only safe to trust once the live set has been written.
		return err
	}
	if err := q.store.Prune(ctx); err != nil {
		return fmt.Errorf("queue: prune store: %w", err)
	}
	return nil
}

func (q *Queue) saveInner(dir string) error {
	// Snapshot job data under RLock, then release before disk I/O.
	// Holding the lock during writeGzJSON would block the entire
	// pipeline (MarkArticleDone, Add, etc.) for the duration of
	// checkpointing — potentially seconds on large queues.
	q.mu.RLock()
	type jobSnapshot struct {
		id   string
		data []byte
	}
	snapshots := make([]jobSnapshot, 0, len(q.jobs))
	jobIDs := make([]string, len(q.jobs))
	paused := q.paused

	for i, job := range q.jobs {
		jobIDs[i] = job.ID
		// Marshal to JSON under the lock so we capture a consistent
		// view. The actual disk I/O happens after unlock.
		data, err := json.Marshal(job) //nolint:gosec // G117: Job.Password is an archive password, not a credential
		if err != nil {
			q.mu.RUnlock()
			return fmt.Errorf("queue: marshal job %s: %w", job.ID, err)
		}
		snapshots = append(snapshots, jobSnapshot{id: job.ID, data: data})
	}
	q.mu.RUnlock()

	// --- No lock held below this line ---

	jobsDir := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o750); err != nil {
		return fmt.Errorf("queue: mkdir %q: %w", jobsDir, err)
	}

	for _, snap := range snapshots {
		if err := writeGzJSONRaw(filepath.Join(jobsDir, snap.id+".json.gz"), snap.data); err != nil {
			return fmt.Errorf("queue: save job %s: %w", snap.id, err)
		}
	}

	idx := indexFile{
		Version: persistenceVersion,
		JobIDs:  jobIDs,
		Paused:  paused,
	}
	if err := writeGzJSON(filepath.Join(dir, "queue.json.gz"), &idx); err != nil {
		return fmt.Errorf("queue: save index: %w", err)
	}
	return nil
}

// newJobProgressSized builds a JobProgress sized to fileArticleCounts (one
// element per file, its article count) without requiring a resident
// Manifest — see Store.ArticleCountsByFile. Used by Loader.Load to give a
// non-resident job (StatusQueued/StatusPaused at restart) a real JobProgress
// instead of leaving it nil.
//
// Every article starts undone/unfailed/unemitted: this sizes progress for
// reporting, it does not restore true per-article state, which needs the
// manifest. That restoration already happens correctly whenever the job is
// actually promoted back to resident — hydrateJobLocked builds a fresh
// newJobProgress(&m) and calls Store.RestoreJobProgress against it — so this
// placeholder only has to survive until then.
//
// remainingBytes is the caller's own byte-accurate figure (from
// Store.RemainingBytesByJob) rather than the job's full total, so a job
// paused mid-download is not misreported as having downloaded nothing
// merely because it restarted non-resident.
func newJobProgressSized(fileArticleCounts []int, remainingBytes int64) *JobProgress {
	total := 0
	for _, c := range fileArticleCounts {
		total += c
	}
	p := &JobProgress{
		done:            newBitset(total),
		failed:          newBitset(total),
		emitted:         newBitset(total),
		files:           make([]FileProgress, len(fileArticleCounts)),
		remainingBytes:  remainingBytes,
		pendingArticles: total,
	}
	for fi, c := range fileArticleCounts {
		p.files[fi].Pending = c
	}
	return p
}

// articleCountsAreLegacy reports whether counts came from a job_files row
// that predates the article_count column (Task 2026-07-31/#267 Task 4): the
// column defaults every existing row to zero, so a non-empty slice that is
// all zeros means "never populated" rather than "genuinely zero articles in
// every file". A job with truly no files at all comes back from
// ArticleCountsByFile as an empty (nil) slice — no rows to scan — which this
// deliberately treats as not legacy: there is nothing to fall back to a
// manifest for.
func articleCountsAreLegacy(counts []int) bool {
	if len(counts) == 0 {
		return false
	}
	for _, c := range counts {
		if c != 0 {
			return false
		}
	}
	return true
}

// Loader reconstructs a Queue from disk with configurable dependencies.
type Loader struct {
	// Rename is used to rename corrupt files to .corrupt.
	// If nil, os.Rename is used.
	Rename func(oldpath, newpath string) error
}

func (l *Loader) rename() func(string, string) error {
	if l.Rename != nil {
		return l.Rename
	}
	return os.Rename
}

func (l *Loader) quarantineFile(path string) error {
	dest := path + ".corrupt"
	return l.rename()(path, dest)
}

// Load reconstructs a Queue from dir. A missing or corrupt queue.json.gz is not
// a fatal error — the daemon will start fresh with an empty queue, and the
// corrupt index is quarantined. Permission errors and quarantine-failure
// errors still propagate.
func Load(dir string, opts ...Option) (*Queue, error) {
	return (&Loader{}).Load(dir, opts...)
}

// Load reconstructs a Queue from dir.
func (l *Loader) Load(dir string, opts ...Option) (*Queue, error) {
	q := New(opts...)
	if q.store != nil {
		q.stateDir = dir
		jobs, err := q.store.List(context.Background())
		if err != nil {
			return nil, fmt.Errorf("queue: load store: %w", err)
		}
		paused, _ := q.store.IsPaused(context.Background())
		// Queried before q.mu.Lock(): Store calls are treated as I/O by
		// scripts/check_lock_io, and must not run inside the critical
		// section below. Fail closed (propagate the error) rather than
		// silently leaving every non-resident job's remaining bytes at
		// zero, matching the fail-closed precedent set for the manifest
		// file-count query in SQLiteStore.Get (#254).
		remainingByJob, err := q.store.RemainingBytesByJob(context.Background())
		if err != nil {
			return nil, fmt.Errorf("queue: load remaining bytes: %w", err)
		}
		// Size JobProgress for every job store.Get left non-resident
		// (progress == nil): Get only restores progress for a
		// resident-status job whose manifest file is present on disk, so
		// every StatusQueued/StatusPaused job — and a resident-status job
		// whose manifest is missing — comes back from List() without one.
		// The invariant this task establishes is job.progress != nil for
		// every job in q.byID (docs/queue-lifecycle.md), so this must run
		// for all of them, not just the subset the loop below re-hydrates.
		//
		// Also queried before q.mu.Lock(), for the same check_lock_io
		// reason as RemainingBytesByJob above: ArticleCountsByJob and the
		// legacy manifest-fallback read/backfill are all I/O. One grouped
		// query for every job rather than one query per job — the same
		// shape RemainingBytesByJob already uses — so a large queued
		// backlog costs one round trip, not N.
		countsByJob, err := q.store.ArticleCountsByJob(context.Background())
		if err != nil {
			return nil, fmt.Errorf("queue: load article counts: %w", err)
		}
		for _, job := range jobs {
			if job.progress != nil {
				continue
			}
			counts := countsByJob[job.ID]
			if articleCountsAreLegacy(counts) {
				// Every count is zero: this job_files row predates Task 4's
				// article_count column. The manifest is the only remaining
				// source of per-file article counts — read it once here
				// rather than leaving progress permanently under-sized.
				//
				// A failed read must degrade, not fail Load: this is boot
				// (Application.Start -> queue.Load), and one damaged manifest
				// must never prevent the daemon from starting — the same
				// rule that keeps the resident-hydration branch below
				// degrading on `if err == nil { ... }` rather than
				// propagating. Falling through with the zero counts sizes
				// this one job's progress to zero articles until the normal
				// claim path (PromoteNext) either hydrates the real manifest
				// or fails the job closed the way a corrupt manifest already
				// does for every other job — exactly the pre-Task-5 outcome,
				// not a new failure mode.
				manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
				var m Manifest
				if err := readGzJSON(manifestPath, &m); err != nil {
					q.log.Warn("legacy article-count fallback: could not read manifest, sizing progress to zero articles",
						"job_id", job.ID, "err", err)
				} else {
					counts = make([]int, m.NumFiles())
					for fi := range counts {
						lo, hi := m.FileRange(fi)
						counts[fi] = hi - lo
					}
					// Write the recovered counts back so this fallback runs
					// once per job, not on every boot. Best-effort: a failed
					// write leaves the row legacy and simply repeats the
					// (already-degraded) fallback next time — it must not
					// turn a successful load into a failed one. Only log
					// "upgraded" once the write actually lands — logging it
					// unconditionally would claim success on the same boot
					// the very next line reports the persist as failed.
					if err := q.store.BackfillArticleCounts(context.Background(), job.ID, counts); err != nil {
						q.log.Warn("legacy article-count fallback: recovered counts from manifest but failed to persist them, will retry on next load",
							"job_id", job.ID, "err", err)
					} else {
						q.log.Info("upgraded legacy job_files row missing article_count", "job_id", job.ID)
					}
				}
			}
			job.progress = newJobProgressSized(counts, remainingByJob[job.ID])
		}
		func() {
			q.mu.Lock()
			defer q.mu.Unlock()
			for _, job := range jobs {
				q.jobs = append(q.jobs, job)
				q.byID[job.ID] = job
				if isResidentStatus(job.Status) && job.manifest == nil {
					manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
					var m Manifest
					if err := readGzJSON(manifestPath, &m); err == nil {
						m.buildMessageIDIndex()
						job.setResidency(&m, newJobProgress(&m))
						job.setScalarsFromManifest(&m)
						_ = q.store.RestoreJobProgress(context.Background(), job)
						q.activeSet.Add(job)
					}
				} else if job.manifest != nil {
					job.manifest.buildMessageIDIndex()
				}
			}
			q.paused = paused
		}()
		_ = q.store.Prune(context.Background())
		return q, nil
	}

	var idx indexFile
	idxPath := filepath.Join(dir, "queue.json.gz")
	err := readGzJSON(idxPath, &idx)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("queue: load index: %w", err)
		}
		// For index, we degrade to empty queue but must quarantine first.
		if qErr := l.quarantineFile(idxPath); qErr != nil {
			return nil, fmt.Errorf("queue: load index failed and could not quarantine: %w (original error: %w)", qErr, err)
		}
		q = New(opts...)
		q.stateDir = dir
		q.log.Warn("quarantining corrupt queue index and degrading to empty queue", "path", idxPath, "err", err)
		return q, nil
	}
	if idx.Version != persistenceVersion {
		return nil, fmt.Errorf("queue: unsupported persistence version %d (expected %d)",
			idx.Version, persistenceVersion)
	}

	q = New(opts...)
	q.stateDir = dir
	q.paused = idx.Paused
	jobsDir := filepath.Join(dir, "jobs")
	for _, id := range idx.JobIDs {
		var job Job
		jobPath := filepath.Join(jobsDir, id+".json.gz")
		if err := readGzJSON(jobPath, &job); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Job file was removed but the index wasn't saved before
				// a crash. Skip the orphaned entry; Prune will clean up.
				continue
			}
			if errors.Is(err, os.ErrPermission) {
				return nil, fmt.Errorf("queue: load job %s: %w", id, err)
			}
			// Quarantine corrupt job file and continue loading others
			if qErr := l.quarantineFile(jobPath); qErr != nil {
				return nil, fmt.Errorf("queue: load job %s failed and could not quarantine: %w (original error: %w)", id, qErr, err)
			}
			q.log.Warn("quarantining corrupt job file", "id", id, "path", jobPath, "err", err)
			continue
		}
		q.jobs = append(q.jobs, &job)
		q.byID[id] = &job
		if m := job.manifest; m != nil {
			job.setScalarsFromManifest(m)
		}
		// Initialize transient counters (Pending, ArticlesResolved,
		// ArticlesFailed) from the loaded done/failed/emitted flags. These
		// are excluded from JSON and must be recomputed after every
		// deserialisation. messageIDIndex is left unbuilt; articleIndexByID
		// builds it lazily the next time it is called.
		job.progress.recompute(job.manifest)
	}
	q.Prune()
	return q, nil
}

// Prune removes orphaned job files in stateDir/jobs/ that are no longer present
// in the queue's index. It also cleans up crash-orphaned temporary files left
// by writeGzJSONRaw's atomic write pattern (temp+fsync+rename).
func (q *Queue) Prune() {
	if q.stateDir == "" {
		return
	}
	jobsDir := filepath.Join(q.stateDir, "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return
	}

	// Collect orphan paths under the lock, then release before disk I/O.
	var toRemove []string
	q.mu.RLock()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Clean up crash-orphaned temp files from atomic writes.
		if strings.Contains(name, ".tmp") {
			toRemove = append(toRemove, filepath.Join(jobsDir, name))
			continue
		}
		if !strings.HasSuffix(name, ".json.gz") {
			continue
		}
		id := strings.TrimSuffix(name, ".json.gz")
		if _, ok := q.byID[id]; !ok {
			toRemove = append(toRemove, filepath.Join(jobsDir, name))
		}
	}
	q.mu.RUnlock()

	for _, p := range toRemove {
		q.log.Info("pruning orphaned job state", "path", p)
		_ = os.Remove(p) // best-effort pruning of orphaned job file
	}
}

// LoadJob reconstructs a single Job from a .json.gz file at path.
func LoadJob(path string) (*Job, error) {
	var job Job
	if err := readGzJSON(path, &job); err != nil {
		return nil, err
	}
	job.progress.recompute(job.manifest)
	return &job, nil
}

// SaveJob serialises a single Job to a .json.gz file at path.
func SaveJob(path string, job *Job) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("queue: mkdir %q: %w", dir, err)
	}
	return writeGzJSON(path, job)
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

// writeGzJSONRaw writes pre-marshalled JSON bytes to a gzipped file at path.
// Used when JSON marshalling happens separately (e.g. under a lock) from disk I/O.
func writeGzJSONRaw(path string, data []byte) error {
	return fsutil.WriteGzAtomicBytes(path, data)
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
