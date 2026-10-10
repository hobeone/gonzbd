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

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// recordReader is what verification reads of the article record.
type recordReader interface {
	FileRows(ctx context.Context, jobID string) ([]durability.FileRow, error)
	WrittenRows(ctx context.Context, jobID string) ([]durability.WrittenRow, error)
}

// appResidency is the production dispatch.Residency: it loads a job's manifest
// from the gzip-JSON file the ingest path wrote, verifies the job's written
// articles against the bytes on disk before attaching anything, and drops the
// manifest again.
//
// It holds no registry of its own. The lookup function is the dispatcher's,
// which is what keeps "which jobs exist" a single owner (Rule 2) rather than
// two maps that can disagree.
type appResidency struct {
	lookup func(string) (*job.Job, bool)
	dir    string
	store  recordReader // nil when there is no history database
	log    *slog.Logger

	// pathFor resolves a recorded filename of the named job to the path its
	// writer used (pipeline.jobFilePath). commit applies verdicts through the
	// recorder's synchronous path. finished receives each file the verifier
	// finished by path, once the job is attached; peek is the archive peek
	// run on each such file before it is marked complete (installVerification),
	// whose failure message finished carries to the Resumed completion;
	// parked receives the fault of a verification that could not complete. All
	// five are set by New before anything can hydrate.
	pathFor  func(jobName, filename string) string
	commit   func(ctx context.Context, j *job.Job, v []durability.FileVerdict) error
	finished func(jobID string, fileIdx int, failMsg string)
	peek     func(j *job.Job, fileIdx int) string
	parked   func(jobID string, f *storagefault.Fault)

	mu        sync.Mutex
	hydrating map[string]chan struct{}
}

func newAppResidency(lookup func(string) (*job.Job, bool), dir string, store recordReader, log *slog.Logger) *appResidency {
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
//
// A job with no progress yet has its written articles verified first
// (verifyJobFiles), and the verdicts committed, before anything is attached:
// if either fails, nothing is attached and the next hydration starts again.
// So a job with progress has always been verified. A verification that could
// not complete for a reason about the device is parked through the stall and
// returned wrapping dispatch.ErrResidencyFault, which the dispatcher does not
// settle.
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

	// A job that has run before in this process has progress already, which
	// is the authoritative record: eviction keeps it, and it was verified
	// when it was first attached. Installing a fresh JobProgress would zero
	// its counters, which is the defect the RestoreContent/AttachContent
	// split exists to prevent.
	if j.HasProgress() {
		return j.RestoreContent(m)
	}
	return r.verifyAndAttach(ctx, j, m)
}

// verifyAndAttach is Hydrate's no-progress branch: read the record, verify it,
// commit the verdicts, and only then attach the content and install what was
// verified.
func (r *appResidency) verifyAndAttach(ctx context.Context, j *job.Job, m *job.Manifest) error {
	if r.store == nil {
		return j.AttachContent(m)
	}
	files, rows, err := r.readRecord(ctx, j.ID())
	if err != nil {
		return r.fault(ctx, j, storagefault.Classify("read record", "", err), err)
	}
	pathFor := func(fn string) string { return r.pathFor(j.Name(), fn) }
	res, err := verifyJobFiles(ctx, m, files, rows, pathFor, false)
	if err != nil {
		f := storagefault.Classify("verify", "", err)
		if vf, ok := errors.AsType[*errVerifyFault](err); ok {
			f = storagefault.Classify("verify", vf.File, vf.Err)
		}
		return r.fault(ctx, j, f, err)
	}
	if len(res.Verdicts) > 0 {
		if err := r.commit(ctx, j, res.Verdicts); err != nil {
			return r.fault(ctx, j, storagefault.Classify("commit verdicts", "", err), err)
		}
	}
	if err := j.AttachContent(m); err != nil {
		return err
	}
	failMsgs := make(map[int]string)
	var peek func(int)
	if r.peek != nil {
		peek = func(fi int) { failMsgs[fi] = r.peek(j, fi) }
	}
	for _, fi := range installVerification(j, files, rows, res, true, r.log, peek) {
		r.finished(j.ID(), fi, failMsgs[fi])
	}
	return nil
}

// readRecord reads a job's job_files and written_articles rows.
func (r *appResidency) readRecord(ctx context.Context, id string) ([]durability.FileRow, []durability.WrittenRow, error) {
	files, err := r.store.FileRows(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	rows, err := r.store.WrittenRows(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return files, rows, nil
}

// fault returns a failed verification's error. A context error is returned
// as it is; anything else parks the job and is marked a residency fault, so
// the dispatcher leaves the job's outcome alone.
func (r *appResidency) fault(ctx context.Context, j *job.Job, f *storagefault.Fault, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("residency: hydrate %s: %w", j.ID(), err)
	}
	r.parked(j.ID(), f)
	return fmt.Errorf("residency: hydrate %s: %w: %w", j.ID(), dispatch.ErrResidencyFault, err)
}

// installVerification installs one verification's outcome on a job whose
// content is attached and has no other record yet, and returns the files the
// verifier finished by path. Each of those is settled (Job.SettleFileCRC),
// peeked and marked complete here, in that order, while the manifest is
// certainly attached; the completion the caller then queues
// (FileComplete.Resumed) runs only the steps that read progress, so it lands
// even if the job is evicted first.
//
// peek, when non-nil, is the archive peek for a finished file. It runs before
// the mark, because the mark is what lets the download-complete report take
// the job to Assessing: a last file marked first could reach post-processing
// unpeeked. A retry passes nil: its job is not registered yet, and the peek
// reads the registered job's unwanted state.
//
// Each file gets its recorded filename, and its recorded fetch policy when
// restorePolicy is set. A complete=1 file's
// rows are installed as Done with every other article of its range failed
// (job.Job.InstallCompleteFile); a complete=0 file gets the rows its read-back
// verified, and the articles an intersection failed. A row that cannot be
// placed costs its own article (Standing Design Rule 3).
func installVerification(j *job.Job, files []durability.FileRow, rows []durability.WrittenRow,
	res verifyResult, restorePolicy bool, log *slog.Logger, peek func(fileIdx int)) (finished []int) {
	byFile := rowsByFile(rows)
	setComplete := make(map[int]bool, len(res.Verdicts))
	for _, v := range res.Verdicts {
		if v.SetComplete {
			setComplete[v.FileIdx] = true
		}
	}
	for _, f := range files {
		fi := f.FileIndex
		_ = j.RestoreFileMeta(fi, f.Filename, false, 0)
		// Hydration restores the persisted policy (restorePolicy); a retry
		// re-derives it. Every mutation of the policy marks the file dirty
		// (markFetchPolicyDirty), so the persisted value is the current one.
		if restorePolicy {
			_ = j.RestoreFetchPolicy(fi, job.FetchPolicy(f.FetchPolicy))
		}

		var dropped int
		var err error
		if f.Complete {
			dropped, err = j.InstallCompleteFile(fi, byFile[fi])
		} else if v := res.Verified[fi]; len(v) > 0 {
			dropped, err = j.InstallVerified(fi, v)
		}
		if err != nil {
			log.Warn("residency: install verified rows", "job", j.ID(), "fileidx", fi, "err", err)
			continue
		}
		if dropped > 0 {
			log.Warn("residency: dropped written rows that do not belong to their file; their articles are fetched again",
				"job", j.ID(), "fileidx", fi, "dropped", dropped)
		}
		for _, a := range res.Failed[fi] {
			_ = j.MarkArticleFailed(int(a))
		}
		if setComplete[fi] {
			if _, _, err := j.SettleFileCRC(fi); err != nil {
				log.Warn("residency: settle a finished file's CRC", "job", j.ID(), "fileidx", fi, "err", err)
			}
			if peek != nil {
				peek(fi)
			}
			if err := j.MarkFileComplete(fi); err != nil {
				log.Warn("residency: mark a finished file complete", "job", j.ID(), "fileidx", fi, "err", err)
				continue
			}
			finished = append(finished, fi)
		}
	}
	return finished
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
