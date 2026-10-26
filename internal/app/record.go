package app

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
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
	// jobs lists the jobs the dispatcher holds, whose fetch-policy changes
	// each flush takes (takeFetchPolicyChanges). Nil takes none.
	jobs func() []*job.Job
	log  *slog.Logger

	// wmu is a deliberate serialising writer lock, held across I/O: flush holds
	// it from its snapshot through ApplyRecord and any remerge, apply holds
	// it from its purge through ApplyRecord, and untrust from its write
	// through the settle. Together they keep an in-flight flush from writing
	// back, or re-merging, rows an untrust has just removed.
	// Lock order: wmu before mu, and wmu before the dispatcher's mu, which
	// current and jobs take. mu and the dispatcher's mu are not nested:
	// current's one caller, liveInstances (through isCurrent), releases mu
	// before calling it, and jobs' one caller, takeFetchPolicyChanges, holds
	// no mu. No caller of flush or apply may hold the dispatcher's mu.
	// noteWritten, markDirty and untrust's purge take only mu; wmu is taken
	// where `git grep -n 'r\.lockWriter(' internal/app/record.go` finds 3
	// lines.
	//
	// It is a 1-buffered channel rather than a sync.Mutex so that each waiter's
	// wait is bounded by its own ctx (lockWriter): a holder with no deadline
	// delays a waiter only until the waiter's ctx ends. Holding it is having
	// sent into it.
	wmu chan struct{}

	mu      sync.Mutex // guards pending, dirty, owed and owedSeq
	pending map[*job.Job][]durability.WrittenRow
	dirty   map[*job.Job]map[int]durability.FileState

	// owed is, per job instance and file, an untrust whose DeleteAll +
	// ClearComplete verdict has not reached SQLite, keyed to the untrust's
	// sequence number (owedSeq, which only untrust advances). untrust adds an
	// entry; a write that carried it settles it (settleLocked) only when the
	// number is still the one it carried, so an untrust that lands while the
	// write is in flight keeps its own debt; the flush drops the entries of an
	// instance the dispatcher no longer holds (owedLocked), whose rows are a
	// retry's to verify or the reclaim's to delete.
	owed    map[*job.Job]map[int]uint64
	owedSeq uint64
}

func newRecorder(st recordStore, current func(id string) *job.Job, jobs func() []*job.Job, log *slog.Logger) *recorder {
	return &recorder{
		st:      st,
		current: current,
		jobs:    jobs,
		log:     log,
		wmu:     make(chan struct{}, 1),
		pending: make(map[*job.Job][]durability.WrittenRow),
		dirty:   make(map[*job.Job]map[int]durability.FileState),
		owed:    make(map[*job.Job]map[int]uint64),
	}
}

// untrustVerdict is the verdict an untrust commits for one file.
func untrustVerdict(fileIdx int) durability.FileVerdict {
	return durability.FileVerdict{FileIdx: fileIdx, DeleteAll: true, ClearComplete: true}
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

// takeFetchPolicyChanges marks dirty every file of a listed job whose fetch
// policy changed since the last take (job.Job.TakeFetchPolicyChanges), so the
// flush that calls it writes the change whichever path made it. It calls jobs
// and the jobs with mu released.
func (r *recorder) takeFetchPolicyChanges() {
	if r.jobs == nil {
		return
	}
	for _, j := range r.jobs() {
		for _, st := range j.TakeFetchPolicyChanges() {
			r.markDirty(j, st.FileIdx, st)
		}
	}
}

// isCurrent is the instance check: a job that is no longer what the dispatcher
// holds under its ID was replaced by a retry.
func (r *recorder) isCurrent(j *job.Job) bool {
	return r.current(j.ID()) == j
}

// liveInstances runs the instance check once for every job with buffered state
// or an owed delete and returns the answers, so every part of a snapshot is
// filtered by the same one. It calls current with mu released. A job first
// buffered after the listing is absent from the result and is left for the
// next flush.
func (r *recorder) liveInstances() map[*job.Job]bool {
	r.mu.Lock()
	seen := make(map[*job.Job]bool, len(r.pending)+len(r.dirty)+len(r.owed))
	jobs := make([]*job.Job, 0, len(r.pending)+len(r.dirty)+len(r.owed))
	add := func(j *job.Job) {
		if !seen[j] {
			seen[j] = true
			jobs = append(jobs, j)
		}
	}
	for j := range r.pending {
		add(j)
	}
	for j := range r.dirty {
		add(j)
	}
	for j := range r.owed {
		add(j)
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

// owedLocked returns a copy of the owed deletes of every current instance
// live has an answer for, keyed to their sequence numbers, and drops those of
// an instance that is not current: a retry under the same ID verifies every
// row of the job before it registers the rebuilt instance, and a job that
// left the queue has its rows deleted by the reclaim rule or kept for such a
// retry. The entries stay owed until settle reports the write that carried
// them succeeded.
func (r *recorder) owedLocked(live map[*job.Job]bool) map[*job.Job]map[int]uint64 {
	out := make(map[*job.Job]map[int]uint64, len(live))
	for j, o := range r.owed {
		current, checked := live[j]
		if !checked {
			continue
		}
		if !current {
			delete(r.owed, j)
			continue
		}
		out[j] = maps.Clone(o)
	}
	return out
}

// settleLocked clears one owed delete, if the untrust that owes it is still
// the one numbered seq: a later untrust of the file replaced the number, and
// the write that carried seq wrote rows that later untrust has since purged.
func (r *recorder) settleLocked(j *job.Job, fileIdx int, seq uint64) {
	if cur, ok := r.owed[j][fileIdx]; !ok || cur != seq {
		return
	}
	delete(r.owed[j], fileIdx)
	if len(r.owed[j]) == 0 {
		delete(r.owed, j)
	}
}

// settle clears every owed delete that a successful write carried.
func (r *recorder) settle(carried map[*job.Job]map[int]uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for j, o := range carried {
		for fi, seq := range o {
			r.settleLocked(j, fi, seq)
		}
	}
}

// overrunLocked reports whether an untrust of file fi of j has been recorded
// since a write carried the debts in carried (none, when carried has no entry
// for fi): what that write snapshotted for the file was noted before that
// untrust and must not be put back.
func (r *recorder) overrunLocked(j *job.Job, carried map[int]uint64, fi int) bool {
	cur, ok := r.owed[j][fi]
	return ok && cur > carried[fi]
}

// lockWriter takes wmu, or returns ctx's error if ctx ends first.
func (r *recorder) lockWriter(ctx context.Context) error {
	select {
	case r.wmu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlockWriter releases wmu; the caller holds it.
func (r *recorder) unlockWriter() { <-r.wmu }

// flush first marks dirty every fetch-policy change the listed jobs hold
// (takeFetchPolicyChanges), then takes ONE snapshot of the pending rows, dirty
// files and owed deletes under mu, so a file's complete flag cannot land
// without the rows noted before it was marked, nor with rows an untrust
// removed, and writes it with mu released but wmu held. On a store error the
// snapshot is merged back for the next flush before wmu is released. If ctx
// ends before wmu is free, it returns ctx's error having taken no snapshot.
func (r *recorder) flush(ctx context.Context) error {
	err := r.lockWriter(ctx)
	if err == nil {
		err = r.flushLocked(ctx) // includes the remerge on error, see flushLocked
		r.unlockWriter()
	}
	if err != nil {
		r.log.Warn("recorder: flush failed; will retry", "err", err)
	}
	return err
}

// flushLocked is flush's body; the caller holds wmu.
func (r *recorder) flushLocked(ctx context.Context) error {
	r.takeFetchPolicyChanges()
	live := r.liveInstances()
	r.mu.Lock()
	rows := r.takeRowsLocked(live)
	files := r.takeFilesLocked(live)
	owed := r.owedLocked(live)
	r.mu.Unlock()

	byJob := make(map[*job.Job]*durability.RecordBatch, len(rows)+len(files)+len(owed))
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
	for j, o := range owed {
		b := batchOf(j)
		for _, fi := range slices.Sorted(maps.Keys(o)) {
			v := untrustVerdict(fi)
			// ApplyRecord runs a verdict's complete change after the batch's
			// file states. A FileState here was marked after the untrust, whose
			// purge dropped the earlier one, and is the file's whole current
			// state, so the verdict leaves complete to it.
			if _, ok := files[j][fi]; ok {
				v.ClearComplete = false
			}
			b.Verdicts = append(b.Verdicts, v)
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
		r.remerge(rows, files, owed)
		return err
	}
	r.settle(owed)
	return nil
}

// remerge puts a failed snapshot back. Snapshot rows go in front of rows that
// arrived since, so the newer row for an article is applied last and wins under
// INSERT OR REPLACE. A FileState is the whole current state of its file rather
// than a delta, so a state marked since the snapshot is newer and is kept; the
// snapshot's is restored only for files with none. Nothing of a file an
// untrust has overrun since the snapshot is put back (overrunLocked): the
// untrust's purge could not reach what the snapshot held, and the rows
// describe bytes it disowned.
func (r *recorder) remerge(rows map[*job.Job][]durability.WrittenRow, files map[*job.Job]map[int]durability.FileState,
	carried map[*job.Job]map[int]uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for j, rs := range rows {
		kept := make([]durability.WrittenRow, 0, len(rs))
		for _, row := range rs {
			if !r.overrunLocked(j, carried[j], row.FileIdx) {
				kept = append(kept, row)
			}
		}
		if len(kept) > 0 {
			r.pending[j] = append(kept, r.pending[j]...)
		}
	}
	for j, fs := range files {
		m := r.dirty[j]
		for idx, st := range fs {
			if r.overrunLocked(j, carried[j], idx) {
				continue
			}
			if m == nil {
				m = make(map[int]durability.FileState, len(fs))
				r.dirty[j] = m
			}
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
// file's buffered one. If ctx ends before wmu is free, it returns ctx's error
// having purged nothing.
func (r *recorder) apply(ctx context.Context, j *job.Job, v []durability.FileVerdict, files ...durability.FileState) error {
	if err := r.lockWriter(ctx); err != nil {
		return err
	}
	defer r.unlockWriter()

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

// untrust removes one file's record: its rows and complete flag in SQLite,
// and whatever is still buffered for it. The buffered state goes first, under
// mu, and the delete is recorded as owed by this instance of j; the delete is
// then tried now, with ctx bounding the wait for wmu and the write. A debt
// the write does not settle is carried, ahead of the file's rows, by every
// later write for the instance (flushLocked) until one succeeds, so no batch
// persists a row noted before the untrust, or complete=1 over such a row,
// while the delete is outstanding. If ctx ends before wmu is free, the purge
// and the debt stand and ctx's error is returned.
//
// The rows of the file noted after the purge are a refetch's: the one caller
// is handleFileUntrusted, on the assembler's worker, which is also the one
// goroutine that notes rows. apply does not carry the debt, and does not
// need to: its callers name an instance that owes nothing — a hydration
// verifies a restored job, which has written no row here, and a retry's
// instance is new (`git grep -n 'recorder\.apply(' -- '*.go' ':!*_test.go'`
// returns 4 lines, the hydration's commit and the retry's three).
func (r *recorder) untrust(ctx context.Context, j *job.Job, fileIdx int) error {
	v := untrustVerdict(fileIdx)
	r.mu.Lock()
	r.purgeLocked(j, v)
	r.owedSeq++
	seq := r.owedSeq
	if r.owed[j] == nil {
		r.owed[j] = make(map[int]uint64)
	}
	r.owed[j][fileIdx] = seq
	r.mu.Unlock()

	if err := r.lockWriter(ctx); err != nil {
		return err
	}
	defer r.unlockWriter()
	if err := r.st.ApplyRecord(ctx, []durability.RecordBatch{{JobID: j.ID(), Verdicts: []durability.FileVerdict{v}}}); err != nil {
		return err
	}
	r.mu.Lock()
	r.settleLocked(j, fileIdx, seq)
	r.mu.Unlock()
	return nil
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
			_ = r.flush(ctx) //nolint:errcheck // flush logs its own failure
		}
	}
}

// nopRecordStore is the recorder's store when there is no history database:
// the record is discarded, and every restart refetches everything.
type nopRecordStore struct{}

func (nopRecordStore) ApplyRecord(context.Context, []durability.RecordBatch) error { return nil }

// recorderFlushTimeout bounds the three flushes whose callers must not block:
// the finalizer's persistAndCommit, the hand-over to post-processing, which
// holds the post-processing admission, and the download-complete report's
// (reportDownloadComplete). It covers the wait for wmu and
// the write. Shutdown's flush has its own step budget instead.
const recorderFlushTimeout = 2 * time.Second

// untrustTimeout bounds the synchronous SQLite untrust of a file: the wait for
// wmu and the write. It runs on the assembler's worker goroutine, which every
// job's writes wait on.
//
// It is shorter than closeHandlesTimeout because the close-handles arm calls
// it inside CloseJobHandles: the caller waits closeHandlesTimeout for the
// worker's reply, and an untrust that used all of it would leave the close
// timed out with its fault unseen. One untrust leaves the close 3s; a close
// with several failing files spends this bound once per file.
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

// queuedJobs is the recorder's job listing: every job the dispatcher holds. A
// job removed between the listing and its lookup is left out.
func (app *Application) queuedJobs() []*job.Job {
	rows := app.dispatcher.List()
	out := make([]*job.Job, 0, len(rows))
	for _, row := range rows {
		if j, ok := app.dispatcher.Job(row.ID); ok {
			out = append(out, j)
		}
	}
	return out
}

// handleArticleWritten is the assembler's Options.OnArticleWritten: the
// article's bytes reached pwrite, so its row is buffered and it is Done, on
// the instance that wrote. An instance that has left the queue buffers under
// itself, which the next flush discards (takeRowsLocked), and its mark lands
// on that instance alone (refused as non-resident once it is evicted).
func (app *Application) handleArticleWritten(owner assembler.Owner, fileIdx int, artIdx int32, off, n int64, crc uint32) {
	j := instanceOf(owner)
	app.recorder.noteWritten(j, durability.WrittenRow{
		FileIdx: fileIdx, ArtIdx: artIdx, Offset: off, Length: n, CRC32: crc,
	})
}

// handleFileUntrusted is the assembler's Options.OnFileUntrusted: a file's
// fsync failed, so none of its written articles can be vouched for. It runs
// synchronously on the assembler's worker, in this order:
//
//  1. The file's rows and complete flag are removed from SQLite through
//     recorder.untrust, which first purges what is still buffered for it, and
//     keeps the delete owed when the write does not land in untrustTimeout.
//     This has to land before the job can be evicted and re-hydrated.
//  2. Its articles return to Outstanding in memory (Job.UntrustFile).
//  3. The pipeline's cached FileInfo is dropped, so the first refetched
//     article re-registers the file with a fresh part count and an empty
//     owned set.
//
// It does not use app.wg: at worker exit, Shutdown may already be waiting on
// it.
//
// It acts only for the instance registered under its ID: the SQLite rows and
// the pipeline's cache are keyed by ID, so an untrust reported for an attempt
// that has left would purge the retry's (#1056).
func (app *Application) handleFileUntrusted(owner assembler.Owner, fileIdx int) {
	j := instanceOf(owner)
	jobID := j.ID()
	if !app.registered(j) {
		app.log.Debug("untrusted file not recorded; the instance has left the queue",
			"job", jobID, "fileidx", fileIdx)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), untrustTimeout)
	defer cancel()
	if err := app.recorder.untrust(ctx, j, fileIdx); err != nil {
		// The delete did not reach SQLite now. The recorder purged the file's
		// buffered rows and state before trying, and owes the delete: every
		// later write for this job instance carries it ahead of the file's
		// rows until one lands, so complete=1 cannot reach SQLite over a row
		// written before the untrust (recorder.untrust). Until then the rows
		// in SQLite have complete=0, and a restart reads each back before
		// trusting it.
		app.log.Error("could not remove an untrusted file's record now; the delete is owed and rides ahead of the file's rows in the recorder's next write",
			"job", jobID, "fileidx", fileIdx, "err", err)
	}
	if err := j.UntrustFile(fileIdx); err != nil {
		app.log.Debug("untrusted file not returned to Outstanding in memory",
			"job", jobID, "fileidx", fileIdx, "err", err)
	} else {
		// The DeleteAll purge dropped the file's whole buffered FileState,
		// filename and fetch policy with it, and registerFile marks a file
		// dirty only when its filename is unset, which UntrustFile leaves set.
		// Without this the next flush writes nothing for the file, and a crash
		// before it completes leaves its later rows under an empty filename.
		app.markFileDirty(j, fileIdx)
	}
	app.pipeline.forgetFile(jobID, fileIdx)
	app.log.Warn("file untrusted after an fsync failure; its articles are fetched again",
		"job", jobID, "fileidx", fileIdx)
}

// enqueueResumedCompletion hands the consumer of internalFileComplete a file
// the verifier finished by path, marked Resumed. It reads app.ctx, so it is
// called only once Start has set it. failMsg is the failure message of the
// archive peek that blocked the job during the hydration that finished the
// file, or "": a job-level fact, so every file that hydration finished carries
// it. Its callers are a hydration's residency.finished and a retry; a
// hydration runs from a dispatcher tick, from a rename
// (Dispatcher.SetName's LoadProgress), and inside Start from
// fileOwedUnwantedFailures.
//
// When the channel is full the send moves to its own goroutine, which gives
// up when app.ctx is cancelled. It cannot block the caller:
// fileOwedUnwantedFailures runs synchronously inside Dispatcher.StartWith,
// before Start launches watchCompletions, so with the channel's 128 slots
// full a blocking send would hang Start. It is not on app.wg: the dispatcher,
// whose tick hydrates, stops
// after Shutdown's app.wg.Wait (joinAndStop), so a wg.Go from a late tick
// could race that Wait. app.ctx is cancelled on every Shutdown of a started
// app (joinAndStop) and on every failed Start, so the goroutine cannot
// outlive the app; a completion it gives up is re-derived by the next start's
// verification.
func (app *Application) enqueueResumedCompletion(j *job.Job, fileIdx int, failMsg string) {
	fc := FileComplete{Job: j, FileIdx: fileIdx, Resumed: true, FailMsg: failMsg}
	select {
	case app.internalFileComplete <- fc:
		return
	default:
	}
	ctx := app.ctx
	app.resumedInFlight.Add(1)
	go func() {
		defer app.resumedInFlight.Add(-1)
		select {
		case app.internalFileComplete <- fc:
		case <-ctx.Done():
			app.log.Info("resumed completion not delivered; the app is stopping",
				"job", j.ID(), "fileidx", fileIdx)
		}
	}()
}

// markFileDirty records one file's current state for the recorder's next
// flush. The state is read from the job's progress, so Complete is true only
// once the file was finished: complete=1 never reaches SQLite before its
// fsync.
func (app *Application) markFileDirty(j *job.Job, fi int) {
	st, ok := j.FileState(fi)
	if !ok {
		return
	}
	app.recorder.markDirty(j, fi, st)
}
