package assembler

import (
	"context"
	"errors"
	"fmt"

	"github.com/hobeone/gonzbd/internal/durability"
)

// fileIdxSyncOp is the control-message FileIdx that carries a barrier request.
//
// It joins the two the assembler already had — -1 cancel, -2 close-handles —
// and works the same way: the caller puts a request on the worker's channel
// and blocks until the worker, which owns every file handle, has done the work
// and answered.
//
// That indirection is invariant X1, not ceremony. One goroutine owns all the
// state, so the barrier can read a file's cache and handle without a lock. The
// alternative — a mutex over the open-file map and the writers — would put
// WriteAt and fsync inside a critical section, which is both a contention
// disaster on the hot path and the thing check_lock_io exists to catch.
const fileIdxSyncOp = -3

// syncOp is one barrier operation for the worker to perform.
type syncOp struct {
	kind    syncOpKind
	jobID   string
	fileIdx int32
	bound   int64
	reply   chan syncReply
}

type syncOpKind int

const (
	opFiles syncOpKind = iota
	opDrain
	opSync
	opStat
	opTruncate
)

// syncReply carries a worker answer back to the barrier's goroutine.
type syncReply struct {
	files     []int32
	written   []durability.WrittenArticle
	size      int64
	modTimeNs int64
	err       error
}

// ErrAssemblerStopped reports a barrier operation submitted after Stop.
var ErrAssemblerStopped = errors.New("assembler: stopped before the barrier operation ran")

// ArticleMap supplies the two manifest facts the assembler does not have and
// cannot derive: how many articles a file holds, and where a global article
// index sits within it.
//
// The assembler deliberately never learns them. It is fed one article at a
// time and has no view of the job's manifest, so any answer it invented would
// be a guess — and the barrier uses these to place bits in a durable bitmap,
// where a wrong ordinal marks the wrong article durable and costs a silently
// short file. The caller that owns the manifest supplies them instead.
type ArticleMap interface {
	// ArticleCount returns how many articles the file holds in total. The
	// barrier needs it to size the durable bitmap at its true width rather
	// than the byte width persistence rounds it up to.
	ArticleCount(fileIdx int32) int
	// FileLocalOrdinal maps a global article index to the file's bit
	// position. False means the article does not belong to the file, which
	// the barrier treats as a bookkeeping defect and fails loudly on rather
	// than skipping the article (A2).
	FileLocalOrdinal(fileIdx, artIdx int32) (int, bool)
}

// SyncTargetFor returns a durability.SyncTarget scoped to one job.
//
// The assembler itself cannot implement SyncTarget: the interface is per-job —
// Files() returns one job's files — while the assembler is keyed on
// fileKey{jobID, fileIdx} and serves every job at once. This adapter supplies
// the missing dimension, and am supplies the manifest facts.
//
// A nil am answers "unknown" to both manifest questions, which makes every
// barrier over this target fail loudly on its first article rather than commit
// a bitmap built from invented ordinals. That is the safe direction, not a
// usable mode.
//
// to supply the per-job dimension durability.SyncTarget requires and the
// multi-job Assembler cannot.
//
//nolint:ireturn // Returning the interface is the point: this accessor exists
func (a *Assembler) SyncTargetFor(jobID string, am ArticleMap) durability.SyncTarget {
	return &jobSyncTarget{a: a, jobID: jobID, am: am}
}

type jobSyncTarget struct {
	a     *Assembler
	jobID string
	am    ArticleMap
}

var _ durability.SyncTarget = (*jobSyncTarget)(nil)

// submit sends one operation to the worker and waits for its answer.
func (t *jobSyncTarget) submit(ctx context.Context, op syncOp) (syncReply, error) {
	op.jobID = t.jobID
	op.reply = make(chan syncReply, 1)

	t.a.mu.Lock()
	if !t.a.started || t.a.stopped {
		t.a.mu.Unlock()
		return syncReply{}, ErrAssemblerStopped
	}
	t.a.wg.Add(1)
	t.a.mu.Unlock()
	defer t.a.wg.Done()

	req := WriteRequest{JobID: "", FileIdx: fileIdxSyncOp, syncOp: &op}
	select {
	case t.a.reqs <- req:
	case <-t.a.stopCh:
		return syncReply{}, ErrAssemblerStopped
	case <-ctx.Done():
		return syncReply{}, ctx.Err()
	}

	select {
	case r := <-op.reply:
		return r, r.err
	case <-ctx.Done():
		return syncReply{}, ctx.Err()
	}
}

// Files returns the job's currently open files. R8 bounds barrier cost by this
// set rather than by job size.
func (t *jobSyncTarget) Files() []int32 {
	// Files has no context in the interface, and the barrier calls it first,
	// before anything can block. Background is honest here: the operation is a
	// map scan on the worker with no I/O in it.
	r, err := t.submit(context.Background(), syncOp{kind: opFiles})
	if err != nil {
		return nil
	}
	return r.files
}

func (t *jobSyncTarget) Drain(ctx context.Context, fileIdx int32) ([]durability.WrittenArticle, error) {
	r, err := t.submit(ctx, syncOp{kind: opDrain, fileIdx: fileIdx})
	return r.written, err
}

func (t *jobSyncTarget) Sync(ctx context.Context, fileIdx int32) error {
	_, err := t.submit(ctx, syncOp{kind: opSync, fileIdx: fileIdx})
	return err
}

func (t *jobSyncTarget) Stat(fileIdx int32) (size, modTimeNs int64, err error) {
	r, e := t.submit(context.Background(), syncOp{kind: opStat, fileIdx: fileIdx})
	return r.size, r.modTimeNs, e
}

// Truncate trims a completed file to bound.
//
// Not part of durability.SyncTarget; the barrier reaches it through a type
// assertion when finalizing a file. See Barrier.FinalizeFile for where the
// bound comes from — it is emphatically NOT this run's high-water mark.
func (t *jobSyncTarget) Truncate(ctx context.Context, fileIdx int32, bound int64) error {
	_, err := t.submit(ctx, syncOp{kind: opTruncate, fileIdx: fileIdx, bound: bound})
	return err
}

// ArticleCount and FileLocalOrdinal come from the manifest, not the worker, so
// they do not go through the control channel at all — they are pure lookups
// against the ArticleMap the caller supplied.
func (t *jobSyncTarget) ArticleCount(fileIdx int32) int {
	if t.am == nil {
		return 0
	}
	return t.am.ArticleCount(fileIdx)
}

func (t *jobSyncTarget) FileLocalOrdinal(fileIdx, artIdx int32) (int, bool) {
	if t.am == nil {
		return 0, false
	}
	return t.am.FileLocalOrdinal(fileIdx, artIdx)
}

// handleSyncOp performs one barrier operation on the worker goroutine.
func (a *Assembler) handleSyncOp(op *syncOp, open map[fileKey]*openFile) {
	var r syncReply
	switch op.kind {
	case opFiles:
		for k := range open {
			if k.jobID == op.jobID {
				r.files = append(r.files, int32(k.fileIdx)) //nolint:gosec // G115: file counts are far below int32
			}
		}
	default:
		key := fileKey{jobID: op.jobID, fileIdx: int(op.fileIdx)}
		f, ok := open[key]
		if !ok {
			// A file the barrier believes open but the worker does not. That
			// is a bookkeeping disagreement, not a storage fault, so it is
			// reported as an error rather than routed through Stallable —
			// blaming storage for it would be the A1 conflation in reverse.
			r.err = fmt.Errorf("assembler: job %s file %d is not open", op.jobID, op.fileIdx)
			break
		}
		switch op.kind {
		case opDrain:
			r.written, r.err = f.w.Drain(context.Background())
		case opSync:
			r.err = f.w.Sync(context.Background())
		case opStat:
			r.size, r.modTimeNs, r.err = f.w.Stat()
		case opTruncate:
			r.err = f.w.Truncate(op.bound)
		case opFiles:
		}
	}
	op.reply <- r
}
