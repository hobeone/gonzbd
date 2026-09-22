package app

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/hobeone/gonzbd/internal/durability"
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
	store     *durability.Store
	log       *slog.Logger
	mu        sync.Mutex
	hydrating map[string]chan struct{}
}

func newAppResidency(lookup func(string) (*job.Job, bool), dir string, store *durability.Store, log *slog.Logger) *appResidency {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &appResidency{
		lookup:    lookup,
		dir:       dir,
		store:     store,
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
	if r.store == nil {
		return
	}
	rows, err := r.store.FileRows(ctx, j.ID())
	if err != nil {
		// Not silent: a file whose row failed to read keeps whatever default
		// state the fresh progress record gave it (not complete, zero CRC,
		// default fetch policy) rather than what was persisted, and nothing
		// downstream can tell the difference. FileRows still returns the rows
		// that did read, and they are applied below.
		r.log.Warn("residency: load job_files", "job", j.ID(), "err", err)
	}
	if j.Progress() == nil {
		return
	}
	for _, f := range rows {
		_ = j.RestoreFileMeta(f.FileIndex, f.Filename, f.Complete, f.AssembledCRC32)
		// Hydration restores the persisted policy, unlike the retry path,
		// which re-derives it. Both restore calls happen adjacently so a
		// reader sees hydration puts back both halves.
		//
		// This overwrites whatever the policy currently is in memory, which is
		// only correct because every mutation of it marks the job: the two par2
		// verdicts go through Application.markFetchPolicyDirty, and ingest
		// derives the policy before the row exists. A future writer that
		// changes the policy without marking would be re-read backwards here on
		// the next eviction, silently.
		_ = j.RestoreFetchPolicy(f.FileIndex, job.FetchPolicy(f.FetchPolicy))
	}
}

func (r *appResidency) restoreResolution(ctx context.Context, j *job.Job) {
	if r.store == nil {
		return
	}
	stored, err := r.store.ForJob(ctx, j.ID())
	if err != nil {
		// A partial read is applied: each run it holds is a durable fact, and
		// a run it misses only leaves those articles to be fetched again.
		r.log.Warn("residency: read durable_runs", "job", j.ID(), "err", err)
		if !errors.Is(err, durability.ErrIncomplete) {
			return
		}
	}
	runs := make([]job.RunRange, len(stored))
	for i, run := range stored {
		runs[i] = job.RunRange{First: run.FirstArtIdx, Last: run.LastArtIdx}
	}

	failed, err := r.store.FailedArticles(ctx, j.ID())
	if err != nil {
		r.log.Warn("residency: read failed_articles", "job", j.ID(), "err", err)
		// A partial read is applied with the runs rather than abandoned:
		// dropping the runs along with it would leave the job with no
		// resolution at all rather than a partial one.
		if !errors.Is(err, durability.ErrIncomplete) {
			return
		}
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
