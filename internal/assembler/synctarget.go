package assembler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/storagefault"
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
	opClose
	opJobs
	opConfirm
)

// String names the operation for a storage fault's Op field, which is what an
// operator reads in a stall reason. Kept beside the constants so a new kind
// cannot be added without a name.
func (k syncOpKind) String() string {
	switch k {
	case opFiles:
		return "list"
	case opDrain:
		return "write"
	case opSync:
		return "sync"
	case opStat:
		return "stat"
	case opTruncate:
		return "truncate"
	case opClose:
		return "close"
	case opJobs:
		return "list"
	case opConfirm:
		return "confirm"
	default:
		return "unknown"
	}
}

// barrierOpTimeout bounds every barrier operation this package submits to the
// worker, without exception: submit applies it, so no caller can opt out and a
// new method cannot be added unbounded.
//
// It once bounded only Files and Stat, on the reasoning that they are "the two
// whose interface signature carries no context". OpenJobIDs does take one and
// was left unbounded anyway, and its caller — the periodic checkpoint loop —
// passes the application's lifetime context, so there was no deadline to
// inherit. Taking a context is not the same as being given a deadline, and
// that is the distinction the old wording missed. Per-caller wrapping is what
// let that happen, so the wrapping moved into submit.
//
// B4 and R22 require every storage syscall on the critical path to be
// timeout-bounded, and the bound has to come from here because the interface
// gives the barrier no context to impose one. A `stat` on a wedged NFS mount
// is exactly the case: the operation runs on the worker goroutine, which owns
// every handle, so an unbounded wait for its reply parks the checkpoint loop
// for as long as the mount stays down and every other job's barrier with it.
//
// What this bounds is the WAIT, not the syscall. Go cannot interrupt a blocked
// fstat, so the worker stays stuck either way — which is the intended
// division: a wedged mount stalls the job, never the process.
//
// It also decides what a timeout MEANS. Drain, Sync and Truncate used to rely
// solely on "the caller's deadline bounds their wait", which made an expiry
// ambiguous: a wedged mount and a shutdown budget produced the identical
// context.DeadlineExceeded, and the barrier classified both as storage. With
// this bound applied underneath the caller's, the two are separable — see
// submit and waitEnded.
//
// Five seconds matches diskCheckTimeout, the other bound this package places
// on a syscall against a possibly-dead mount.
const barrierOpTimeout = 5 * time.Second

// syncReply carries a worker answer back to the barrier's goroutine.
type syncReply struct {
	files     []int32
	jobs      []string
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
// A nil am makes the target report NO FILES, so Barrier.Run over it iterates
// nothing. Note the scope of that claim: Run is the only entry point that asks
// for the file set. Barrier.FinalizeFile takes an explicit file index and never
// calls Files(), so this alone does NOT make a nil-am target inert — an earlier
// version of this comment said it did, which was a true statement about one
// path generalised to a path it did not cover.
//
// What actually closes the hole is Barrier.buildExtent refusing to build an
// extent for a target reporting no articles. That is the chokepoint both entry
// points pass through, and its doc explains the erasure it prevents. This guard
// remains as the cheaper outer layer, not as the guarantee.
//
// Nil is for a caller that has no manifest and therefore has no business
// running a barrier; it is not a usable mode.
//
//nolint:ireturn // the adapter's whole purpose is to return the interface
func (a *Assembler) SyncTargetFor(jobID string, am ArticleMap) durability.SyncTarget {
	return &jobSyncTarget{a: a, jobID: jobID, am: am}
}

type jobSyncTarget struct {
	a     *Assembler
	jobID string
	am    ArticleMap
}

var _ durability.SyncTarget = (*jobSyncTarget)(nil)

// errWorkerUnresponsive is what a barrierOpTimeout expiry means: the worker
// did not answer, which on the paths that reach here means it is parked in a
// syscall against a mount that is not responding.
//
// A named error rather than context.DeadlineExceeded so the reason survives
// into the stall text an operator reads, where "context deadline exceeded"
// says nothing about what to fix.
var errWorkerUnresponsive = errors.New("the assembler worker did not answer within " + barrierOpTimeout.String())

// submit sends one operation to the worker and waits for its answer.
//
// # Every wait is bounded here, and every failure is named
//
// The timeout is applied in this one place rather than by each caller. Callers
// that wrapped their own context and callers that did not were the same
// function to the worker, and OpenJobIDs — reached from the single loop that
// also owns stall re-evaluation and the queue save — was one of the ones that
// did not. Owning it here means a new method cannot be added unbounded.
//
// The reply select carries a stopCh case for the same reason the send select
// always did. Without it a caller whose context has no deadline waits forever
// on a worker parked in an fsync, and the deferred wg.Done never runs, so
// Stop's wg.Wait blocks behind it and the process cannot exit.
//
// # What the error MEANS is decided here, not by errno matching downstream
//
// durability.ErrTargetUnavailable states the boundary rule: return a
// *storagefault.Fault or a wrapped sentinel, never a bare error. Handing the
// barrier a raw ctx.Err() broke it — storagefault.Classify matches no errno for
// a context error, defaults to retryable, and Stallable parked a healthy job
// on a disk that did not fail. The commonest trigger was not exotic: an
// ordinary shutdown, whose checkpoint budget expires on a queue with many open
// files.
//
// So the two timeouts are distinguished rather than conflated. OUR bound
// expiring is evidence about storage and becomes a fault; the CALLER's context
// ending is evidence about the caller and becomes the sentinel.
func (t *jobSyncTarget) submit(ctx context.Context, op syncOp) (syncReply, error) {
	op.jobID = t.jobID
	op.reply = make(chan syncReply, 1)

	t.a.mu.Lock()
	if !t.a.started || t.a.stopped {
		t.a.mu.Unlock()
		return syncReply{}, t.unavailable(ErrAssemblerStopped)
	}
	t.a.wg.Add(1)
	t.a.mu.Unlock()
	defer t.a.wg.Done()

	opCtx, cancel := context.WithTimeout(ctx, barrierOpTimeout)
	defer cancel()

	req := WriteRequest{JobID: "", FileIdx: fileIdxSyncOp, syncOp: &op}
	select {
	case t.a.reqs <- req:
	case <-t.a.stopCh:
		return syncReply{}, t.unavailable(ErrAssemblerStopped)
	case <-opCtx.Done():
		return syncReply{}, t.waitEnded(ctx, op)
	}

	select {
	case r := <-op.reply:
		return r, r.err
	case <-t.a.stopCh:
		return syncReply{}, t.unavailable(ErrAssemblerStopped)
	case <-opCtx.Done():
		return syncReply{}, t.waitEnded(ctx, op)
	}
}

// unavailable wraps a condition that is not about storage, so the barrier's
// boundary check recognises it. See durability.ErrTargetUnavailable.
func (t *jobSyncTarget) unavailable(err error) error {
	return fmt.Errorf("%w: %w", durability.ErrTargetUnavailable, err)
}

// waitEnded names why an operation's wait ended without an answer.
//
// The caller's context ending is the caller's decision — a shutdown budget, a
// per-job checkpoint budget — and says nothing about the device. Our own bound
// expiring with the caller's context still live is the wedged-mount case, and
// that one has to reach Stallable or the job sits at 99% with nothing
// surfaced, which A2 forbids.
func (t *jobSyncTarget) waitEnded(caller context.Context, op syncOp) error {
	if err := caller.Err(); err != nil {
		return t.unavailable(err)
	}
	// No path. Resolving one means calling Options.FileInfo, and this is the
	// path taken precisely when something is not answering — the resolver
	// reaches the queue, which can hydrate a manifest from disk. Blocking the
	// timeout handler on the condition it is reporting is how a bound stops
	// being a bound. Barrier.raise fills the path in; it already has it, and
	// it is not the thing that is stuck.
	return storagefault.Classify(op.kind.String(), "", errWorkerUnresponsive)
}

// Files returns the job's currently open files. R8 bounds barrier cost by this
// set rather than by job size.
func (t *jobSyncTarget) Files() []int32 {
	// No manifest, no files. See SyncTargetFor: this is what keeps a nil
	// ArticleMap inert instead of destructive.
	if t.am == nil {
		return nil
	}
	files, err := t.a.OpenFiles(context.Background(), t.jobID)
	if err != nil {
		// Reporting no files makes the barrier a no-op, which is safe — it
		// claims nothing — but it is not nothing happening, so it is not
		// silent. Without this the job simply stops checkpointing and the
		// only symptom is rework after a restart.
		//
		// A stopped assembler is the exception: it is the ordinary end of
		// every process, and every completion still in flight at that point
		// goes through here.
		//
		// This method cannot report the difference — durability.SyncTarget
		// gives it no error to return, deliberately, because the barrier has
		// nothing useful to do with one. A caller that DOES need to tell a
		// wedged mount from an ordinary shutdown must call OpenFiles itself;
		// Application.finalizeCompletedFile is the one that does, and the
		// reason is that proceeding on a timeout there ships an untrimmed
		// file.
		if errors.Is(err, ErrAssemblerStopped) {
			t.a.log.Debug("barrier asked a stopped assembler for a job's files", "job", t.jobID)
			return nil
		}
		t.a.log.Warn("barrier could not list a job's open files; this checkpoint claims nothing",
			"job", t.jobID, "err", err)
		return nil
	}
	return files
}

// OpenFiles returns the job's currently open files, or an error saying why it
// could not find out.
//
// It is the error-returning form of the SyncTarget adapter's Files, and it
// exists because "no files" and "could not tell" are different answers with
// different consequences. A caller deciding whether a completed file has been
// trimmed must not read a barrierOpTimeout on a wedged mount as "there was
// nothing to trim" — that ships pre-allocation's trailing zeros to par2 as
// damage, which is #350 by another route.
//
// The bound is barrierOpTimeout for the same reason Files carries one: the
// operation is a map scan with no I/O in it, but it still has to reach the
// worker, and the worker can be blocked in someone else's fsync on a dead
// mount.
func (a *Assembler) OpenFiles(ctx context.Context, jobID string) ([]int32, error) {
	t := &jobSyncTarget{a: a, jobID: jobID}
	r, err := t.submit(ctx, syncOp{kind: opFiles})
	if err != nil {
		return nil, err
	}
	return r.files, nil
}

// Path returns the file's on-disk path for a stall reason a user can act on
// (R27), or "" when it cannot be resolved.
//
// It deliberately does NOT go through the worker. Path is called from the
// barrier's fault-routing path, and a wedged worker is precisely the condition
// that gets it called, so asking the worker would be asking the thing that is
// stuck. Options.FileInfo is the same resolver the worker itself used to open
// the file, and it is a pure lookup with no I/O.
func (t *jobSyncTarget) Path(fileIdx int32) string {
	info, err := t.a.opts.FileInfo(t.jobID, int(fileIdx))
	if err != nil {
		return ""
	}
	return info.Path
}

func (t *jobSyncTarget) Drain(ctx context.Context, fileIdx int32) ([]durability.WrittenArticle, error) {
	r, err := t.submit(ctx, syncOp{kind: opDrain, fileIdx: fileIdx})
	return r.written, err
}

func (t *jobSyncTarget) Sync(ctx context.Context, fileIdx int32) error {
	_, err := t.submit(ctx, syncOp{kind: opSync, fileIdx: fileIdx})
	return err
}

// Stat reads the file's size and mtime, bounded by barrierOpTimeout because
// the interface hands it no context to bound it with. See that constant.
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

// CloseFile drains, fsyncs, and closes one completed file's handle, and blocks
// until the worker has done so.
//
// It exists because the assembler no longer closes a file the moment its last
// part arrives. The barrier's FinalizeFile has to Drain, Sync, Truncate and
// Stat that file, and every one of those needs the handle the assembler owns —
// so closing at completion would leave the completed file untrimmed and its
// last articles unacked, which is #342/#350 by a different route.
//
// The handoff is therefore: the worker tombstones the file and reports it
// complete, the caller finalizes it through the barrier, and then calls this.
// Closing a file that is already gone — CancelJob, CloseJobHandles or shutdown
// got there first — is a no-op rather than an error.
// The wait is bounded by barrierOpTimeout on top of whatever the caller's
// context already imposes. Releasing a handle is the LAST thing the completion
// path does, and it runs on the single goroutine that consumes every file
// completion for every job — so an unbounded wait here parks that consumer for
// as long as a wedged mount stays down, and no job's files complete again. The
// handle then leaks, which is the lesser of the two costs and the one the
// worker's own shutdown drain still cleans up.
func (a *Assembler) CloseFile(ctx context.Context, jobID string, fileIdx int32) error {
	t := &jobSyncTarget{a: a, jobID: jobID}
	_, err := t.submit(ctx, syncOp{kind: opClose, fileIdx: fileIdx})
	return err
}

// Confirm releases the file's drain report on the worker goroutine that owns
// it, once the barrier's commit and ack have both landed.
//
// Failures are swallowed rather than returned, and the interface makes that
// explicit by declaring no error. A Confirm that does not arrive — a wedged
// worker, a stopped assembler — leaves the report in place, and the next Drain
// re-reports it, which R12 requires the apply to absorb. There is nothing for a
// caller to do about it that the retry does not already do.
func (t *jobSyncTarget) Confirm(ctx context.Context, fileIdx int32) {
	if _, err := t.submit(ctx, syncOp{kind: opConfirm, fileIdx: fileIdx}); err != nil {
		t.a.log.Debug("confirm the drain report", "job", t.jobID, "fileidx", fileIdx, "err", err)
	}
}

// OpenJobIDs returns every job that currently holds an open file.
//
// The checkpoint cadence uses it to pick which jobs to run a barrier for. The
// set comes from here rather than from job status because "has an open file"
// is this package's fact and R8 bounds barrier cost by exactly that set — a
// barrier fsyncs open files, not every file a job will eventually produce.
// Deriving it from the queue instead would be a second representation of one
// fact, free to drift (S5).
func (a *Assembler) OpenJobIDs(ctx context.Context) ([]string, error) {
	// Bounded like every other helper here, and for a consequence wider than
	// theirs. This one is called from the periodic checkpoint loop, which is a
	// single select that also owns the stall re-evaluation tick and the queue
	// save; an unbounded wait for a worker parked in an fsync stops all three,
	// for every job. That is the process-wide freeze "a wedged mount stalls the
	// job, never the process" exists to rule out. submit owns the bound now;
	// this comment stays because the consequence of losing it is the widest
	// here.
	t := &jobSyncTarget{a: a}
	r, err := t.submit(ctx, syncOp{kind: opJobs})
	return r.jobs, err
}

// handleSyncOp performs one barrier operation on the worker goroutine.
func (a *Assembler) handleSyncOp(op *syncOp, open map[fileKey]*openFile, wc *writeCache) {
	var r syncReply
	switch op.kind {
	case opFiles:
		for k := range open {
			if k.jobID == op.jobID {
				r.files = append(r.files, int32(k.fileIdx)) //nolint:gosec // G115: file counts are far below int32
			}
		}
	case opJobs:
		seen := make(map[string]struct{}, len(open))
		for k := range open {
			if _, dup := seen[k.jobID]; dup {
				continue
			}
			seen[k.jobID] = struct{}{}
			r.jobs = append(r.jobs, k.jobID)
		}
	default:
		key := fileKey{jobID: op.jobID, fileIdx: int(op.fileIdx)}
		f, ok := open[key]
		if !ok {
			if op.kind == opClose {
				// Closing a file that is already gone is the expected
				// outcome of a race with CancelJob, CloseJobHandles or
				// shutdown, not a disagreement. Idempotent, not an error.
				break
			}
			// A file the barrier believes open but the worker does not. That
			// is a bookkeeping disagreement, not a storage fault, so it is
			// reported as an error rather than routed through Stallable —
			// blaming storage for it would be the A1 conflation in reverse.
			//
			// Saying so was not enough on its own: the barrier wrapped every
			// target error in storagefault.Classify, so this reached Stallable
			// anyway and paused a healthy job. The sentinel is what makes the
			// distinction legible on the other side of the interface.
			// Wrapped in durability.ErrFileNotOpen so the barrier can tell
			// this apart from a storage failure. Without the sentinel it
			// classified this as one, and a job whose file had merely been
			// closed by a completed finalize was paused with a device reason
			// no operator could act on.
			r.err = fmt.Errorf("assembler: job %s file %d is not open: %w",
				op.jobID, op.fileIdx, durability.ErrFileNotOpen)
			break
		}
		switch op.kind {
		case opDrain:
			r.written, r.err = f.w.Drain()
			// The barrier routes the fault; it cannot route the ARTICLES. A
			// failed drain rolls back every article after the write that
			// failed, and that set never crosses the SyncTarget interface —
			// so without this they are neither Done, nor Failed, nor
			// Outstanding, and only a restart recovers them.
			a.releaseFaulted(f, op.jobID, int(op.fileIdx))
		case opSync:
			r.err = f.w.Sync()
		case opConfirm:
			f.w.Confirm()
		case opStat:
			r.size, r.modTimeNs, r.err = f.w.Stat()
		case opTruncate:
			r.err = f.w.Truncate(op.bound)
		case opClose:
			// Answered, not swallowed. This arm used to leave r.err nil, so
			// CloseFile reported success for a close whose Drain, Sync or
			// Close had failed and both callers carried on — the file marked
			// complete and fed to DirectUnpack and post-processing while its
			// bytes were not all on disk. opDrain, opSync and opTruncate all
			// answer their caller; this one now does too.
			//
			// The callers still only LOG it, deliberately. By the time the
			// barrier closes a file it has drained, synced, truncated,
			// committed the extent and acked the articles, so a fault from
			// this redundant second fsync is post-hoc: acting on it would race
			// the completion it is part of. Reporting it is what makes it
			// visible; deciding what it means belongs to the caller.
			r.err = a.drainAndClose(f)
			delete(open, key)
			wc.forget(key)
		case opFiles, opJobs:
		}
	}
	op.reply <- r
}
