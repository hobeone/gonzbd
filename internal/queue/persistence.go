package queue

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// Save serialises the queue to dir. Layout:
//
//	dir/queue.json.gz         - index (job order + paused flag)
//	dir/jobs/<id>.json.gz     - one per job
//
// Each write is atomic (temp file + fsync + rename). Jobs are written
// first so that a crash between them and the index leaves recoverable
// state: the stale index points at jobs that now exist, and
// unreferenced job files are ignored by Load.
//
// The dirty flag is swapped to false before the write begins. Any
// concurrent mutation that fires after the swap sets dirty=true again,
// so the next checkpoint will pick it up. If the save itself fails,
// dirty is set back to true so the next tick retries.
func (q *Queue) Save(dir string) error {
	// Swap dirty=false before writing. Any mutation that races this
	// will set dirty=true again; if the save fails we restore it so
	// the next checkpoint tick retries rather than skipping.
	q.dirty.Store(false)

	if err := q.saveInner(dir); err != nil {
		q.dirty.Store(true)
		return err
	}
	q.mu.Lock()
	q.stateDir = dir
	q.mu.Unlock()
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
	var idx indexFile
	idxPath := filepath.Join(dir, "queue.json.gz")
	err := readGzJSON(idxPath, &idx)
	if errors.Is(err, os.ErrNotExist) {
		return New(opts...), nil
	}
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("queue: load index: %w", err)
		}
		// For index, we degrade to empty queue but must quarantine first.
		if qErr := l.quarantineFile(idxPath); qErr != nil {
			return nil, fmt.Errorf("queue: load index failed and could not quarantine: %w (original error: %w)", qErr, err)
		}
		q := New(opts...)
		q.stateDir = dir
		q.log.Warn("quarantining corrupt queue index and degrading to empty queue", "path", idxPath, "err", err)
		return q, nil
	}
	if idx.Version != persistenceVersion {
		return nil, fmt.Errorf("queue: unsupported persistence version %d (expected %d)",
			idx.Version, persistenceVersion)
	}

	q := New(opts...)
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
