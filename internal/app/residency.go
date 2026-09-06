package app

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/hobeone/gonzbd/internal/job"
)

// appResidency is the production dispatch.Residency: it loads a job's manifest
// from the gzip-JSON file the ingest path wrote, and drops it again.
//
// It holds no registry of its own. The lookup function is the dispatcher's,
// which is what keeps "which jobs exist" a single owner (Rule 2) rather than
// two maps that can disagree.
type appResidency struct {
	lookup    func(string) (*job.Job, bool)
	dir       string
	db        *sql.DB
	log       *slog.Logger
	mu        sync.Mutex
	hydrating map[string]chan struct{}
}

func newAppResidency(lookup func(string) (*job.Job, bool), dir string, db *sql.DB, log *slog.Logger) *appResidency {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &appResidency{
		lookup:    lookup,
		dir:       dir,
		db:        db,
		log:       log,
		hydrating: make(map[string]chan struct{}),
	}
}

// Hydrate loads the manifest and attaches it. It may block on disk I/O; the
// dispatcher calls it with no lock held (ports.go).
func (r *appResidency) Hydrate(ctx context.Context, id string) error {
	j, ok := r.lookup(id)
	if !ok {
		return fmt.Errorf("residency: hydrate %s: no such job", id)
	}

	r.mu.Lock()
	if r.hydrating == nil {
		r.hydrating = make(map[string]chan struct{})
	}
	if ready, ok := r.hydrating[id]; ok {
		r.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if !j.Resident() {
			return fmt.Errorf("residency: hydrate %s: concurrent hydration failed", id)
		}
		return nil
	}
	if j.Resident() {
		r.mu.Unlock()
		return nil
	}
	ready := make(chan struct{})
	r.hydrating[id] = ready
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.hydrating, id)
		close(ready)
		r.mu.Unlock()
	}()

	m, err := r.readManifest(ctx, id)
	if err != nil {
		return fmt.Errorf("residency: hydrate %s: %w", id, err)
	}

	// A job that has run before has progress already; installing a fresh
	// JobProgress would zero its counters, which is the defect the
	// RestoreContent/AttachContent split exists to prevent.
	if p := j.Progress(); p != nil {
		if err := j.RestoreContent(m, p); err != nil {
			return err
		}
	} else if err := j.AttachContent(m); err != nil {
		return err
	}
	r.restoreJobFiles(ctx, j)
	r.restoreResolution(ctx, j)
	return nil
}

func (r *appResidency) restoreJobFiles(ctx context.Context, j *job.Job) {
	if r.db == nil {
		return
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT file_index, filename, complete, assembled_crc32, fetch_policy FROM job_files WHERE job_id = ?`, j.ID())
	if err != nil {
		r.log.Warn("residency: load job_files", "job", j.ID(), "err", err)
		return
	}
	defer func() { _ = rows.Close() }()
	p := j.Progress()
	if p == nil {
		return
	}
	for rows.Next() {
		var fi int
		var filename string
		var complete int
		var crc uint32
		var fetch int
		if err := rows.Scan(&fi, &filename, &complete, &crc, &fetch); err != nil {
			// Not silent: this file keeps whatever default state the fresh
			// progress record gave it (not complete, zero CRC, default fetch
			// policy) rather than what was persisted, and nothing downstream
			// can tell the difference.
			r.log.Warn("residency: scan job_files", "job", j.ID(), "err", err)
			continue
		}
		_ = j.RestoreFileMeta(fi, filename, complete != 0, crc, job.FetchPolicy(fetch)) //nolint:gosec // G115: fetch_policy is 0-2, fits in uint8
	}
	// rows.Next() returns false for "no more rows" AND for a mid-iteration
	// fault, so without this a dropped connection reads as a complete result
	// set and the job hydrates with silently truncated file metadata.
	if err := rows.Err(); err != nil {
		r.log.Warn("residency: iterate job_files", "job", j.ID(), "err", err)
	}
}

func (r *appResidency) restoreResolution(ctx context.Context, j *job.Job) {
	if r.db == nil {
		return
	}
	runsRows, err := r.db.QueryContext(ctx,
		`SELECT first_art_idx, last_art_idx FROM durable_runs WHERE job_id = ?`, j.ID())
	if err != nil {
		r.log.Warn("residency: query durable_runs", "job", j.ID(), "err", err)
		return
	}
	defer func() { _ = runsRows.Close() }()

	var runs []job.RunRange
	for runsRows.Next() {
		var rr job.RunRange
		if err := runsRows.Scan(&rr.First, &rr.Last); err != nil {
			r.log.Warn("residency: scan durable_runs", "job", j.ID(), "err", err)
			return
		}
		runs = append(runs, rr)
	}
	if err := runsRows.Err(); err != nil {
		r.log.Warn("residency: iterate durable_runs", "job", j.ID(), "err", err)
	}

	failedRows, err := r.db.QueryContext(ctx,
		`SELECT art_idx FROM failed_articles WHERE job_id = ?`, j.ID())
	if err != nil {
		r.log.Warn("residency: query failed_articles", "job", j.ID(), "err", err)
		return
	}
	defer func() { _ = failedRows.Close() }()

	var failed []int32
	for failedRows.Next() {
		var idx int32
		if err := failedRows.Scan(&idx); err != nil {
			// break, not return: the durable runs read moments ago are already
			// in hand, and returning here would discard them along with the
			// failed articles, leaving the job with no resolution at all
			// rather than a partial one.
			r.log.Warn("residency: scan failed_articles", "job", j.ID(), "err", err)
			break
		}
		failed = append(failed, idx)
	}
	if err := failedRows.Err(); err != nil {
		r.log.Warn("residency: iterate failed_articles", "job", j.ID(), "err", err)
	}

	if err := j.ApplyResolution(runs, failed); err != nil {
		r.log.Warn("residency: apply resolution", "job", j.ID(), "err", err)
	}
}

// Evict drops the manifest. Progress stays resident by design.
func (r *appResidency) Evict(id string) {
	r.mu.Lock()
	if ready, ok := r.hydrating[id]; ok {
		r.mu.Unlock()
		<-ready
		r.mu.Lock()
	}
	defer r.mu.Unlock()
	j, ok := r.lookup(id)
	if !ok {
		return
	}
	j.Evict()
}

func (r *appResidency) readManifest(_ context.Context, id string) (*job.Manifest, error) {
	f, err := openManifestIn(r.dir, id)
	if err != nil {
		return nil, err
	}
	return decodeManifest(f)
}

// decodeManifest reads and unmarshals a gzipped JSON job manifest, closing f.
//
// It takes an already-open file rather than a path so that every caller reaches
// a manifest through openManifestIn's os.Root, which confines the job ID to the
// manifest directory at the syscall.
//
// The read is untimed. AdminDir is ordinarily local, but a remote NFS/SMB mount
// can stall it — docs/durability-contract.md carries that failure class — so a
// caller under a deadline gets no help from this function.
func decodeManifest(f *os.File) (*job.Manifest, error) {
	defer func() { _ = f.Close() }()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var m job.Manifest
	if err := json.NewDecoder(zr).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}
