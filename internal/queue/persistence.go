package queue

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
		data, err := json.Marshal(job)
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

// Load reconstructs a Queue from dir. A missing queue.json.gz is not
// an error — the daemon is starting fresh and an empty queue is
// returned. Any other I/O or decode error propagates.
func Load(dir string) (*Queue, error) {
	var idx indexFile
	err := readGzJSON(filepath.Join(dir, "queue.json.gz"), &idx)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue: load index: %w", err)
	}
	if idx.Version != persistenceVersion {
		return nil, fmt.Errorf("queue: unsupported persistence version %d (expected %d)",
			idx.Version, persistenceVersion)
	}

	q := New()
	q.stateDir = dir
	q.paused = idx.Paused
	jobsDir := filepath.Join(dir, "jobs")
	for _, id := range idx.JobIDs {
		var job Job
		if err := readGzJSON(filepath.Join(jobsDir, id+".json.gz"), &job); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Job file was removed but the index wasn't saved before
				// a crash. Skip the orphaned entry; Prune will clean up.
				continue
			}
			return nil, fmt.Errorf("queue: load job %s: %w", id, err)
		}
		q.jobs = append(q.jobs, &job)
		q.byID[id] = &job
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
		slog.Info("pruning orphaned job state", "path", p)
		_ = os.Remove(p)
	}
}

// LoadJob reconstructs a single Job from a .json.gz file at path.
func LoadJob(path string) (*Job, error) {
	var job Job
	if err := readGzJSON(path, &job); err != nil {
		return nil, err
	}
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

// writeGzJSON encodes v as gzipped JSON and atomically publishes it
// at path. Uses the same temp+fsync+rename dance as config.Save so a
// crash at any point leaves either the old file or the new file
// intact, never a half-written mix.
func writeGzJSON(path string, v any) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()        //nolint:errcheck // best-effort cleanup on error path
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on error path
	}

	gz := gzip.NewWriter(tmp)
	enc := json.NewEncoder(gz)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		cleanup()
		return fmt.Errorf("encode: %w", err)
	}
	if err := gz.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close gzip: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on error path
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on error path
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// writeGzJSONRaw writes pre-marshalled JSON data to a gzipped file at path.
// Uses the same atomic temp+fsync+rename dance as writeGzJSON. This variant
// is used when JSON marshalling is done separately (e.g. under a lock) from
// the disk I/O.
func writeGzJSONRaw(path string, data []byte) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()        //nolint:errcheck // best-effort cleanup on error path
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on error path
	}

	gz := gzip.NewWriter(tmp)
	if _, err := gz.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write gzip: %w", err)
	}
	if err := gz.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close gzip: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on error path
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on error path
		return fmt.Errorf("rename: %w", err)
	}
	return nil
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
