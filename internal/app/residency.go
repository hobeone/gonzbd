package app

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/job"
)

// appResidency is the production dispatch.Residency: it loads a job's manifest
// from the gzip-JSON file the ingest path wrote, and drops it again.
//
// It holds no registry of its own. The lookup function is the dispatcher's,
// which is what keeps "which jobs exist" a single owner (Rule 2) rather than
// two maps that can disagree.
type appResidency struct {
	lookup func(string) (*job.Job, bool)
	dir    string
	log    *slog.Logger
}

func newAppResidency(lookup func(string) (*job.Job, bool), dir string, log *slog.Logger) *appResidency {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &appResidency{lookup: lookup, dir: dir, log: log}
}

// Hydrate loads the manifest and attaches it. It may block on disk I/O; the
// dispatcher calls it with no lock held (ports.go).
func (r *appResidency) Hydrate(ctx context.Context, id string) error {
	j, ok := r.lookup(id)
	if !ok {
		return fmt.Errorf("residency: hydrate %s: no such job", id)
	}
	if j.Resident() {
		return nil
	}

	m, err := r.readManifest(ctx, id)
	if err != nil {
		return fmt.Errorf("residency: hydrate %s: %w", id, err)
	}

	// A job that has run before has progress already; installing a fresh
	// JobProgress would zero its counters, which is the defect the
	// RestoreContent/AttachContent split exists to prevent.
	if p := j.Progress(); p != nil {
		return j.RestoreContent(m, p)
	}
	return j.AttachContent(m)
}

// Evict drops the manifest. Progress stays resident by design.
func (r *appResidency) Evict(id string) {
	j, ok := r.lookup(id)
	if !ok {
		return
	}
	j.Evict()
}

func (r *appResidency) readManifest(_ context.Context, id string) (*job.Manifest, error) {
	path := filepath.Join(r.dir, id+".json.gz")
	f, err := os.Open(path) //nolint:gosec // path is dir + a validated job ID
	if err != nil {
		return nil, err
	}
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
