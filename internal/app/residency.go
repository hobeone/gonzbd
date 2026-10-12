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
	"github.com/hobeone/gonzbd/internal/keyedlatch"
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

	// locate resolves a recorded filename of the named job to the directory
	// and name its writer used (pipeline.jobFileLocation). commit applies verdicts through the
	// recorder's synchronous path. finished receives each file the verifier
	// finished by path, once the job is attached; peek is the archive peek
	// run on each such file before it is marked complete (installVerification),
	// whose failure message finished carries to the Resumed completion;
	// parked receives the fault of a verification that could not complete. All
	// five are set by New before anything can hydrate.
	locate   func(jobName, filename string) jobFile
	commit   func(ctx context.Context, j *job.Job, v []durability.FileVerdict) error
	finished func(jobID string, fileIdx int, failMsg string)
	peek     func(j *job.Job, fileIdx int) string
	parked   func(jobID string, f *storagefault.Fault)

	// hydrating holds a claim on each job ID a Hydrate is in flight for,
	// released when it returns; cancels holds the cancel of the context it
	// runs under, which is how Evict ends it rather than waiting for its
	// reads. Under mu, an ID is in cancels exactly while a hydration holds
	// its claim. Both change only under mu
	// (`git grep -n 'r\.hydrating\.\(TryClaim\|Release\)(' -- 'internal/app/*.go' ':!*_test.go'`
	// returns 3 lines: the claim, the release of a claim taken for a job
	// already resident, and the deferred release). The resident job's claim
	// is taken and given back inside one mu span and never enters cancels, so
	// no holder of mu sees it.
	mu        sync.Mutex
	hydrating keyedlatch.Latch
	cancels   map[string]context.CancelFunc
}

func newAppResidency(lookup func(string) (*job.Job, bool), dir string, store recordReader, log *slog.Logger) *appResidency {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &appResidency{
		lookup:  lookup,
		dir:     dir,
		store:   store,
		log:     log,
		cancels: make(map[string]context.CancelFunc),
	}
}

// Hydrate loads the manifest and attaches it. It may block on disk I/O; the
// dispatcher calls it with no lock held (ports.go).
//
// A job with no progress yet has its written articles verified first
// (verifyJobFiles), and the verdicts committed, before anything is attached:
// if either fails, nothing is attached and the next hydration starts again.
// So a job Hydrate attached was verified first. Other jobs gain progress
// without Hydrate: a freshly ingested job has no rows to verify, and a retry
// verifies its rebuilt job itself (Application.verifyRetry). A verification
// that could not complete for a reason about the device is parked through the
// stall and returned wrapping dispatch.ErrResidencyFault, which the dispatcher
// does not settle.
//
// One hydration of a job runs at a time. A call that finds one in flight
// waits for it, and is done if it attached; if it did not — it was cancelled,
// by its caller or by Evict, its own deadline passed, or it faulted — the
// waiter starts its own, under its own ctx. The waiter does not report the
// other's failure as its own: Dispatcher.reconcileResidency settles a job
// Failed on a hydration error that is neither a context error nor a residency
// fault, so a tick's hydration that waited on the startup verification of a
// job the user resumed (Application.verifyPausedJobs) must not turn that
// verification's deadline into such an error.
func (r *appResidency) Hydrate(ctx context.Context, id string) error {
	j, ok := r.lookup(id)
	if !ok {
		return fmt.Errorf("residency: hydrate %s: no such job", id)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for {
		r.mu.Lock()
		if r.hydrating.TryClaim(id) {
			// Taken and, for a resident job, given back in one mu span, so
			// Evict, which looks under mu, never finds it; a waiter that
			// finds it wakes at once and goes round again.
			if j.Resident() {
				r.hydrating.Release(id)
				r.mu.Unlock()
				return nil
			}
			if r.cancels == nil {
				r.cancels = make(map[string]context.CancelFunc)
			}
			r.cancels[id] = cancel
			r.mu.Unlock()
			break
		}
		r.mu.Unlock()
		if err := r.hydrating.Wait(ctx, id); err != nil {
			return err
		}
	}

	defer func() {
		r.mu.Lock()
		delete(r.cancels, id)
		r.hydrating.Release(id)
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
	locate := func(fn string) jobFile { return r.locate(j.Name(), fn) }
	res, err := verifyJobFiles(ctx, m, files, rows, locate, false)
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
	// The message is a fact about the job, not the file: only the peek that
	// moved the job to Blocked returns one, and the first completion queued
	// may be an unflagged file's, which would file the job without the names.
	// So the first message goes to every completion of this hydration.
	var failMsg string
	var peek func(int)
	if r.peek != nil {
		peek = func(fi int) {
			if msg := r.peek(j, fi); failMsg == "" {
				failMsg = msg
			}
		}
	}
	for _, fi := range installVerification(j, files, rows, res, true, r.log, peek) {
		r.finished(j.ID(), fi, failMsg)
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
// the dispatcher leaves the job's outcome alone. An error that comes back
// after ctx ended is returned wrapping ctx's error, whatever it carries
// itself: Evict cancels a tick's hydration while the tick's own context is
// live, and Dispatcher.reconcileResidency settles the job Failed on a
// hydration error it cannot identify as a context error.
func (r *appResidency) fault(ctx context.Context, j *job.Job, f *storagefault.Fault, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("residency: hydrate %s: %w: %w", j.ID(), ctxErr, err)
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("residency: hydrate %s: %w", j.ID(), err)
	}
	r.parked(j.ID(), f)
	return fmt.Errorf("residency: hydrate %s: %w: %w", j.ID(), dispatch.ErrResidencyFault, err)
}

// installVerification installs one verification's outcome on a job whose
// content is attached and has no other record yet, and returns the files the
// verifier finished by path. Each file's outcome is applied in one step under
// the job's content lock (job.Job.InstallFileVerification), which owns the
// order within it: the recorded filename; the recorded fetch policy when
// restorePolicy is set; the rows, which for a complete=1 file are all its rows
// with every other article of its range failed, and for a complete=0 file the
// rows its read-back verified; the articles an intersection failed; and, for
// a complete=1 file or one the verifier finished, the settled CRC. A row that cannot be placed costs its own
// article (Standing Design Rule 3). A hydration restores the policy because
// the record is the current truth; a retry re-derives it. The policy's one
// writer records every change it makes, and the recorder's next flush writes
// it (job.Job.TakeFetchPolicyChanges), so the persisted value catches up to
// memory within a flush. A restored hold over a release is set as FetchAlways
// instead (see JobProgress.setFileFetchPolicy).
//
// Each finished file is then peeked and marked complete
// (job.Job.MarkFileComplete), in that order, while the manifest is certainly
// attached; the completion the caller then queues (FileComplete.Resumed) runs
// only the steps that read progress, so it lands even if the job is evicted
// first.
//
// peek, when non-nil, is the archive peek for a finished file. It runs with no
// job lock held, between the install and the mark, because the mark is what
// lets the download-complete report take the job to Assessing: a last file
// marked first could reach post-processing unpeeked. A retry passes nil: its
// job is not registered yet, and the peek reads the registered job's unwanted
// state.
//
// Failed bits are in memory only, so what a restart re-derives is all there is:
// a complete=1 file's failed set is the complement of its rows, while a
// complete=0 file's earlier permanent failures are not restored — those
// articles are Outstanding and are fetched again.
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
		v := job.FileVerification{
			FileIdx:       fi,
			Filename:      f.Filename,
			RestorePolicy: restorePolicy,
			Policy:        job.FetchPolicy(f.FetchPolicy),
			Complete:      f.Complete,
			Rows:          res.Verified[fi],
			Failed:        res.Failed[fi],
			Settle:        setComplete[fi],
		}
		if f.Complete {
			v.Rows = byFile[fi]
		}
		dropped, err := j.InstallFileVerification(v)
		if err != nil {
			log.Warn("residency: install verified rows", "job", j.ID(), "fileidx", fi, "err", err)
			continue
		}
		if dropped > 0 {
			log.Warn("residency: dropped written rows that do not belong to their file; their articles are fetched again",
				"job", j.ID(), "fileidx", fi, "dropped", dropped)
		}
		if setComplete[fi] {
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
//
// A hydration of the job in flight is cancelled and then waited for, and so
// is any that a waiting Hydrate starts in its place while Evict waits, so that
// none is in flight when it drops the manifest. A waiting Hydrate that wakes
// after Evict has dropped it may still start another. The wait ends when the
// hydration returns: its manifest read returns at the cancellation
// (readManifest) and its read-back checks the context between rows, but a
// read-back read blocked in the syscall holds the wait until it returns. That
// read is not abandoned as the manifest read is, because what follows it may
// truncate or fsync the file (verifyJobFiles).
func (r *appResidency) Evict(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		// Done, not Wait: the channel is taken under mu with the cancel, so
		// Evict waits on the hydration it cancelled and not on one that
		// replaced it after mu was released.
		done := r.hydrating.Done(id)
		if done == nil {
			break
		}
		r.cancels[id]()
		r.mu.Unlock()
		<-done
		r.mu.Lock()
	}
	j, ok := r.lookup(id)
	if !ok {
		return
	}
	j.Evict()
}

// readManifest opens and decodes a job's manifest, and returns ctx's error as
// soon as ctx ends even if the read has not.
//
// Closing the file would not end a read blocked in the syscall: os.File.Close
// on a regular file releases the descriptor only once the read in flight
// returns. So the open and decode run on their own goroutine, which a
// cancelled caller leaves behind; it writes nothing, and it closes the file
// when the read returns.
func (r *appResidency) readManifest(ctx context.Context, id string) (*job.Manifest, error) {
	type result struct {
		m   *job.Manifest
		err error
	}
	done := make(chan result, 1)
	go func() {
		f, err := openManifestIn(r.dir, id)
		if err != nil {
			done <- result{err: err}
			return
		}
		m, err := decodeManifest(f)
		done <- result{m: m, err: err}
	}()
	select {
	case res := <-done:
		return res.m, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// decodeManifest reads and unmarshals a gzipped JSON job manifest, closing f.
//
// It takes an already-open file rather than a path so that every caller reaches
// a manifest through openManifestIn's os.Root, which confines the job ID to the
// manifest directory at the syscall.
//
// The read is untimed. AdminDir is ordinarily local, but a remote NFS/SMB mount
// can stall it — docs/durability-contract.md carries that failure class — so a
// caller under a deadline gets no help from this function; readManifest runs
// it on a goroutine it can abandon.
func decodeManifest(f *os.File) (*job.Manifest, error) {
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only manifest file

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
