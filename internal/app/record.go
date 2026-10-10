package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// recordStore is the slice of durability.Store the recorder writes through.
type recordStore interface {
	ApplyRecord(ctx context.Context, batches []durability.RecordBatch) error
}

// recorder buffers one row per written article and one FileState per dirty
// file, keyed by job instance, and writes them in one transaction per flush.
// Verdicts go through apply, which takes the same writer lock as flush.
type recorder struct {
	st      recordStore
	current func(id string) *job.Job
	log     *slog.Logger

	// wmu is a deliberate serialising writer lock, held across I/O: flush holds
	// it from its snapshot through ApplyRecord and any remerge, and apply holds
	// it from its purge through ApplyRecord. Together they keep an in-flight
	// flush from writing back, or re-merging, rows an untrust has just removed.
	// Lock order: wmu before mu, and wmu before the dispatcher's mu, which
	// current takes. mu and the dispatcher's mu are not nested: current's one
	// caller, liveInstances (through isCurrent), releases mu before calling
	// it. No caller of flush or apply may hold the dispatcher's mu. noteWritten and markDirty take only mu; wmu is
	// taken where `git grep -n 'r\.wmu\.Lock()' internal/app/record.go` finds 2 lines.
	wmu sync.Mutex

	mu      sync.Mutex // guards pending and dirty
	pending map[*job.Job][]durability.WrittenRow
	dirty   map[*job.Job]map[int]durability.FileState
}

func newRecorder(st recordStore, current func(id string) *job.Job, log *slog.Logger) *recorder {
	return &recorder{
		st:      st,
		current: current,
		log:     log,
		pending: make(map[*job.Job][]durability.WrittenRow),
		dirty:   make(map[*job.Job]map[int]durability.FileState),
	}
}

// noteWritten is the OnArticleWritten handler body: the row is appended first,
// so nothing persisted depends on the Done bit, then the article is marked
// done and its row kept resident for the whole-file CRC.
func (r *recorder) noteWritten(j *job.Job, row durability.WrittenRow) {
	r.mu.Lock()
	r.pending[j] = append(r.pending[j], row)
	r.mu.Unlock()

	err := j.MarkArticleWritten(row)
	switch {
	case err == nil:
	case errors.Is(err, job.ErrNotResident):
		r.log.Debug("recorder: article written for a non-resident job", "job", j.ID(), "art", row.ArtIdx)
	default:
		r.log.Warn("recorder: mark article done failed", "job", j.ID(), "art", row.ArtIdx, "err", err)
	}
}

// markDirty records the latest state of one file; a later call for the same
// file replaces an earlier one.
func (r *recorder) markDirty(j *job.Job, fileIdx int, st durability.FileState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.dirty[j]
	if m == nil {
		m = make(map[int]durability.FileState)
		r.dirty[j] = m
	}
	st.FileIdx = fileIdx
	m[fileIdx] = st
}

// isCurrent is the instance check: a job that is no longer what the dispatcher
// holds under its ID was replaced by a retry.
func (r *recorder) isCurrent(j *job.Job) bool {
	return r.current(j.ID()) == j
}

// liveInstances runs the instance check once for every job with buffered state
// and returns the answers, so both halves of a snapshot are filtered by the
// same one. It calls current with mu released. A job first buffered after the
// listing is absent from the result and is left for the next flush.
func (r *recorder) liveInstances() map[*job.Job]bool {
	r.mu.Lock()
	jobs := make([]*job.Job, 0, len(r.pending)+len(r.dirty))
	for j := range r.pending {
		jobs = append(jobs, j)
	}
	for j := range r.dirty {
		if _, ok := r.pending[j]; !ok {
			jobs = append(jobs, j)
		}
	}
	r.mu.Unlock()

	live := make(map[*job.Job]bool, len(jobs))
	for _, j := range jobs {
		live[j] = r.isCurrent(j)
	}
	return live
}

// takeRowsLocked removes from pending the rows of every job live has an answer
// for, and returns those of current instances.
func (r *recorder) takeRowsLocked(live map[*job.Job]bool) map[*job.Job][]durability.WrittenRow {
	out := make(map[*job.Job][]durability.WrittenRow, len(live))
	for j, rows := range r.pending {
		current, checked := live[j]
		if !checked {
			continue
		}
		delete(r.pending, j)
		if current {
			out[j] = rows
		}
	}
	return out
}

// takeFilesLocked is takeRowsLocked for dirty file states.
func (r *recorder) takeFilesLocked(live map[*job.Job]bool) map[*job.Job]map[int]durability.FileState {
	out := make(map[*job.Job]map[int]durability.FileState, len(live))
	for j, files := range r.dirty {
		current, checked := live[j]
		if !checked {
			continue
		}
		delete(r.dirty, j)
		if current {
			out[j] = files
		}
	}
	return out
}

// flush takes ONE snapshot of the pending rows and dirty files under mu, so a
// file's complete flag cannot land without the rows noted before it was
// marked, and writes it with mu released but wmu held. On a store error the
// snapshot is merged back for the next flush before wmu is released.
func (r *recorder) flush(ctx context.Context) error {
	r.wmu.Lock()
	err := r.flushLocked(ctx) // includes the remerge on error, see flushLocked
	r.wmu.Unlock()
	if err != nil {
		r.log.Warn("recorder: flush failed; will retry", "err", err)
	}
	return err
}

// flushLocked is flush's body; the caller holds wmu.
func (r *recorder) flushLocked(ctx context.Context) error {
	live := r.liveInstances()
	r.mu.Lock()
	rows := r.takeRowsLocked(live)
	files := r.takeFilesLocked(live)
	r.mu.Unlock()

	byJob := make(map[*job.Job]*durability.RecordBatch, len(rows)+len(files))
	batchOf := func(j *job.Job) *durability.RecordBatch {
		b := byJob[j]
		if b == nil {
			b = &durability.RecordBatch{JobID: j.ID()}
			byJob[j] = b
		}
		return b
	}
	for j, rs := range rows {
		batchOf(j).Rows = rs
	}
	for j, fs := range files {
		b := batchOf(j)
		for _, f := range fs {
			b.Files = append(b.Files, f)
		}
	}
	if len(byJob) == 0 {
		return nil
	}
	batches := make([]durability.RecordBatch, 0, len(byJob))
	for _, b := range byJob {
		batches = append(batches, *b)
	}
	if err := r.st.ApplyRecord(ctx, batches); err != nil {
		// The remerge must finish before wmu is released: otherwise an apply
		// that was waiting could purge first and this remerge would then
		// resurrect the untrusted rows.
		r.remerge(rows, files)
		return err
	}
	return nil
}

// remerge puts a failed snapshot back. Snapshot rows go in front of rows that
// arrived since, so the newer row for an article is applied last and wins under
// INSERT OR REPLACE. A FileState is the whole current state of its file rather
// than a delta, so a state marked since the snapshot is newer and is kept; the
// snapshot's is restored only for files with none.
func (r *recorder) remerge(rows map[*job.Job][]durability.WrittenRow, files map[*job.Job]map[int]durability.FileState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for j, rs := range rows {
		r.pending[j] = append(slices.Clip(rs), r.pending[j]...)
	}
	for j, fs := range files {
		m := r.dirty[j]
		if m == nil {
			m = make(map[int]durability.FileState, len(fs))
			r.dirty[j] = m
		}
		for idx, st := range fs {
			if _, newer := m[idx]; !newer {
				m[idx] = st
			}
		}
	}
}

// apply commits verdicts, and optionally whole file states, for j
// synchronously. j comes from the caller and current is not consulted: a retry
// commits before the rebuilt job is added to the dispatcher. It holds wmu from
// the purge through ApplyRecord, so no flush is in flight that could write
// back what the verdict removes, and the purge removes what is still
// buffered: a DeleteAll verdict the file's pending rows, a DeleteArtIdxs
// verdict the named pending rows, and any verdict that sets or clears
// complete the file's dirty state. A file state given here replaces the
// file's buffered one.
func (r *recorder) apply(ctx context.Context, j *job.Job, v []durability.FileVerdict, files ...durability.FileState) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()

	r.mu.Lock()
	for _, fv := range v {
		r.purgeLocked(j, fv)
	}
	if m := r.dirty[j]; m != nil {
		for _, f := range files {
			delete(m, f.FileIdx)
		}
		if len(m) == 0 {
			delete(r.dirty, j)
		}
	}
	r.mu.Unlock()
	return r.st.ApplyRecord(ctx, []durability.RecordBatch{{JobID: j.ID(), Verdicts: v, Files: files}})
}

func (r *recorder) purgeLocked(j *job.Job, fv durability.FileVerdict) {
	if fv.DeleteAll || len(fv.DeleteArtIdxs) > 0 {
		drop := make(map[int32]bool, len(fv.DeleteArtIdxs))
		for _, a := range fv.DeleteArtIdxs {
			drop[a] = true
		}
		kept := r.pending[j][:0]
		for _, row := range r.pending[j] {
			if row.FileIdx == fv.FileIdx && (fv.DeleteAll || drop[row.ArtIdx]) {
				continue
			}
			kept = append(kept, row)
		}
		if len(kept) == 0 {
			delete(r.pending, j)
		} else {
			r.pending[j] = kept
		}
	}
	m := r.dirty[j]
	switch st, ok := m[fv.FileIdx]; {
	case fv.DeleteAll:
		delete(m, fv.FileIdx)
	case ok && (fv.SetComplete || fv.ClearComplete):
		st.Complete = fv.SetComplete
		m[fv.FileIdx] = st
	}
	if len(m) == 0 {
		delete(r.dirty, j)
	}
}

// run flushes every interval until ctx is done. It does not flush on cancel.
func (r *recorder) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.flush(ctx) // flush logs its own failure
		}
	}
}

// nopRecordStore is the recorder's store when there is no history database:
// the record is discarded, and every restart refetches everything.
type nopRecordStore struct{}

func (nopRecordStore) ApplyRecord(context.Context, []durability.RecordBatch) error { return nil }

// untrustTimeout bounds the synchronous SQLite write that untrusts a file. It
// runs on the assembler's worker goroutine, which every job's writes wait on.
//
// It is shorter than closeHandlesTimeout because the close-handles arm calls
// it inside CloseJobHandles: the caller waits closeHandlesTimeout for the
// worker's reply, and an untrust that used all of it would leave the close
// timed out with its fault unseen. One untrust leaves the close 3s; a close
// with several failing files spends this bound once per file. It does not
// cover the wait for wmu behind a flush already writing.
const untrustTimeout = 2 * time.Second

// lookupCurrent is the recorder's instance check: the job the dispatcher holds
// under id, or nil.
func (app *Application) lookupCurrent(id string) *job.Job {
	j, ok := app.dispatcher.Job(id)
	if !ok {
		return nil
	}
	return j
}

// handleArticleWritten is the assembler's Options.OnArticleWritten: the
// article's bytes reached pwrite, so its row is buffered and it is Done.
func (app *Application) handleArticleWritten(jobID string, fileIdx int, artIdx int32, off, n int64, crc uint32) {
	j, ok := app.dispatcher.Job(jobID)
	if !ok {
		app.log.Debug("written article not recorded; the job has left the queue",
			"job", jobID, "fileidx", fileIdx, "artidx", artIdx)
		return
	}
	app.recorder.noteWritten(j, durability.WrittenRow{
		FileIdx: fileIdx, ArtIdx: artIdx, Offset: off, Length: n, CRC32: crc,
	})
}

// handleFileUntrusted is the assembler's Options.OnFileUntrusted: a file's
// fsync failed, so none of its written articles can be vouched for. It runs
// synchronously on the assembler's worker, in this order:
//
//  1. The file's rows and complete flag are removed from SQLite through the
//     recorder's synchronous path, which also purges what is still buffered
//     for it. This has to land before the job can be evicted and re-hydrated.
//  2. Its articles return to Outstanding in memory (Job.UntrustFile).
//  3. The pipeline's cached FileInfo is dropped, so the first refetched
//     article re-registers the file with a fresh part count and an empty
//     owned set.
//
// It does not use app.wg: at worker exit, Shutdown may already be waiting on
// it.
func (app *Application) handleFileUntrusted(jobID string, fileIdx int) {
	j, ok := app.dispatcher.Job(jobID)
	if !ok {
		app.log.Debug("untrusted file not recorded; the job has left the queue",
			"job", jobID, "fileidx", fileIdx)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), untrustTimeout)
	defer cancel()
	if err := app.recorder.apply(ctx, j, []durability.FileVerdict{
		{FileIdx: fileIdx, DeleteAll: true, ClearComplete: true},
	}); err != nil {
		// The purge above already dropped the file's buffered rows and
		// complete flag, so what stays in SQLite is rows flushed earlier, with
		// complete still 0: complete=1 is written only after a successful
		// fsync. A complete=0 row is trusted at the next start only after
		// verification reads its bytes back and matches its CRC.
		app.log.Error("could not remove an untrusted file's record; the next start reads its rows back and checks each CRC before trusting them",
			"job", jobID, "fileidx", fileIdx, "err", err)
	}
	if err := j.UntrustFile(fileIdx); err != nil {
		app.log.Debug("untrusted file not returned to Outstanding in memory",
			"job", jobID, "fileidx", fileIdx, "err", err)
	}
	app.pipeline.forgetFile(jobID, fileIdx)
	app.log.Warn("file untrusted after an fsync failure; its articles are fetched again",
		"job", jobID, "fileidx", fileIdx)
}

// enqueueResumedCompletion hands the consumer of internalFileComplete a file
// the verifier finished by path, marked Resumed.
func (app *Application) enqueueResumedCompletion(jobID string, fileIdx int) {
	fc := FileComplete{JobID: jobID, FileIdx: fileIdx, Resumed: true}
	select {
	case app.internalFileComplete <- fc:
	default:
		app.wg.Go(func() { app.internalFileComplete <- fc })
	}
}

// syncFileHandle is the assembler's Options.SyncFile: the syncFile seam when a
// test set one, otherwise the handle's own fsync.
func (app *Application) syncFileHandle(f *os.File) error {
	if app.syncFile != nil {
		return app.syncFile(f)
	}
	return f.Sync()
}

// markFileDirty records one file's current state for the recorder's next
// flush. The state is read from the job's progress, so Complete is true only
// once the file was finished: complete=1 never reaches SQLite before its
// fsync.
func (app *Application) markFileDirty(j *job.Job, fi int) {
	p := j.Progress()
	if p == nil || fi < 0 || fi >= p.NumFiles() {
		return
	}
	app.recorder.markDirty(j, fi, durability.FileState{
		Complete:    p.FileComplete(fi),
		Filename:    p.FileFilename(fi),
		FetchPolicy: uint8(p.FileFetchPolicy(fi)),
	})
}
