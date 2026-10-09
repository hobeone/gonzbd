package app

import (
	"context"
	"errors"
	"log/slog"
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
// Verdicts go through apply, the same writer, so nothing here is a second
// path to written_articles or the job_files updates.
type recorder struct {
	st      recordStore
	current func(id string) *job.Job
	log     *slog.Logger

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
// so nothing persisted depends on the Done bit, then the article is marked done.
func (r *recorder) noteWritten(j *job.Job, row durability.WrittenRow, bytes int64, server string) {
	r.mu.Lock()
	r.pending[j] = append(r.pending[j], row)
	r.mu.Unlock()

	err := j.MarkArticleDone(int(row.ArtIdx), bytes, server)
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

// takeRowsLocked empties pending, dropping rows of replaced instances.
func (r *recorder) takeRowsLocked() map[*job.Job][]durability.WrittenRow {
	out := make(map[*job.Job][]durability.WrittenRow, len(r.pending))
	for j, rows := range r.pending {
		if r.isCurrent(j) {
			out[j] = rows
		}
	}
	r.pending = make(map[*job.Job][]durability.WrittenRow)
	return out
}

// takeFilesLocked empties dirty, dropping files of replaced instances.
func (r *recorder) takeFilesLocked() map[*job.Job]map[int]durability.FileState {
	out := make(map[*job.Job]map[int]durability.FileState, len(r.dirty))
	for j, files := range r.dirty {
		if r.isCurrent(j) {
			out[j] = files
		}
	}
	r.dirty = make(map[*job.Job]map[int]durability.FileState)
	return out
}

// flush takes ONE snapshot of the pending rows and dirty files under mu, so a
// file's complete flag cannot land without the rows noted before it was
// marked, and writes it with mu released. On a store error the snapshot is
// merged back for the next flush.
func (r *recorder) flush(ctx context.Context) error {
	r.mu.Lock()
	rows := r.takeRowsLocked()
	files := r.takeFilesLocked()
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
		r.remerge(rows, files)
		r.log.Warn("recorder: flush failed; will retry", "err", err)
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

// apply commits verdicts for j synchronously. j comes from the caller and
// current is not consulted: a retry commits its verdict before the rebuilt
// job is added to the dispatcher. A DeleteAll verdict first purges the file's
// pending rows and dirty state, and a DeleteArtIdxs verdict the named
// pending rows, so a later background flush cannot write them back.
func (r *recorder) apply(ctx context.Context, j *job.Job, v []durability.FileVerdict) error {
	r.mu.Lock()
	for _, fv := range v {
		r.purgeLocked(j, fv)
	}
	r.mu.Unlock()
	return r.st.ApplyRecord(ctx, []durability.RecordBatch{{JobID: j.ID(), Verdicts: v}})
}

func (r *recorder) purgeLocked(j *job.Job, fv durability.FileVerdict) {
	if !fv.DeleteAll && len(fv.DeleteArtIdxs) == 0 {
		return
	}
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
	r.pending[j] = kept
	if fv.DeleteAll {
		delete(r.dirty[j], fv.FileIdx)
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
