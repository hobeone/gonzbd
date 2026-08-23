package durability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// Barrier is the single place the Written → Durable → Resolved transition
// happens (X2). Its two methods Run and FinalizeFile are the only callers of
// newProof, so no other code path — inside this package or out — can ack an
// article THROUGH Queue.AckDurable.
//
// It is also, since the durable-runs change, the single WRITER of the
// durability record itself. RunStore.Commit is called from exactly two places,
// both below, both inside the transaction that precedes the ack. Resumer no
// longer writes anything: it deletes a file's runs when the file on disk
// contradicts them and otherwise reads. The queue's seeding entry points, which
// used to take a fully exported FileExtent and reach markDone with no barrier
// and no proof, are gone with the type — what installs a resume's answer now
// carries runs the store produced rather than a bitmap any package could build.
//
// Before this design the assembler could ack from six places, and the same
// defect was refiled twice (#355, #356). One place is the whole point;
// adding a second caller of newProof silently undoes it.
type Barrier struct {
	runs  RunStore
	ack   Acker
	stall Stallable
	log   *slog.Logger

	// reportedMu guards reported and nothing else. It is never held across
	// any of this type's I/O — see NewBarrier.
	reportedMu sync.Mutex
	// reported latches the files whose overlap has already been raised.
	//
	// An overlap is a property of the PERSISTED runs, so every checkpoint after
	// the first re-derives the same finding from the same rows. Without a
	// latch a job with one malformed file raises it on every cycle until the
	// download ends — and because Queue.SetWarning holds a single string, each
	// re-raise also overwrites whatever warning was written in between.
	//
	// Keyed on job and file because Run's caller serialises per JOB, so two
	// jobs genuinely do run here at once.
	//
	// Entries are removed only by ForgetJob, which a retry calls because it
	// reuses the job ID. Nothing reclaims them on ordinary completion, and the
	// bound that makes that acceptable is the number of files carrying an
	// overlap — a defect count, not a job count. It is deliberately
	// per-process: a restart raises each finding once more, which is what a
	// user who restarted to fix something would expect.
	reported map[overlapKey]struct{}
}

// overlapKey identifies the file an overlap finding was raised for.
type overlapKey struct {
	jobID   string
	fileIdx int32
}

// NewBarrier wires a barrier. It owns none of its collaborators' lifecycles.
//
// It holds one lock, reportedMu, and that lock never covers I/O: it is taken
// and released around a map probe on the report latch, below every collaborator
// call. Run and FinalizeFile do I/O throughout and the project bans I/O under a
// lock, so nothing else here may grow a lock without moving the I/O first.
func NewBarrier(rs RunStore, ack Acker, stall Stallable, log *slog.Logger) *Barrier {
	return &Barrier{
		runs: rs, ack: ack, stall: stall, log: log,
		reported: make(map[overlapKey]struct{}),
	}
}

// admit filters classified findings down to the ones not yet raised for their
// file, and latches those.
//
// The single owner of the latch — both Run and FinalizeFile go through here, so
// neither can latch on a different rule, and there is nowhere else to write the
// map.
//
// Called only AFTER a successful Commit, never at the point of classification.
// Spending the latch is irreversible for the life of the process, so it must
// not happen on a path that then fails: classify freely, admit once the cycle
// has actually landed. Both an earlier draft of this change and the first fix
// to it got that order wrong, in each case leaving a window where the finding
// was consumed by a cycle that reported nothing.
func (b *Barrier) admit(jobID string, cands []PostAnomaly) []PostAnomaly {
	if len(cands) == 0 {
		return nil
	}
	b.reportedMu.Lock()
	defer b.reportedMu.Unlock()
	var out []PostAnomaly
	for _, pa := range cands {
		key := overlapKey{jobID: jobID, fileIdx: pa.FileIdx}
		if _, seen := b.reported[key]; seen {
			continue
		}
		b.reported[key] = struct{}{}
		out = append(out, pa)
	}
	return out
}

// ForgetJob drops every latched overlap finding for a job, so its next
// checkpoint may raise them again.
//
// Called when a job re-enters the queue under an ID it has already used — a
// retry reuses the job ID, and its durable runs are usually retained across it:
// job_finalizer declines to delete them for a FAILED job, and RetryHistoryJob
// uses DeleteKeepingDurability. They are dropped deliberately when the
// re-parsed manifest changes shape, because a run names articles by art_idx
// and a renumbering makes it describe other articles.
//
// Without this the retried job's overlaps match the latch from the previous
// attempt and are dropped, silencing the warning permanently rather than once.
//
// So ForgetJob may not assume any run is present, and does not need to: it
// clears the latch either way, which is correct for both outcomes.
//
// Idempotent, and safe for a job that never reported anything.
func (b *Barrier) ForgetJob(jobID string) {
	b.reportedMu.Lock()
	defer b.reportedMu.Unlock()
	for key := range b.reported {
		if key.jobID == jobID {
			delete(b.reported, key)
		}
	}
}

// Run executes one checkpoint for a job:
//
//	drain → fsync → commit the runs → ack
//
// The order is the invariant (S1). Nothing before the fsync may be claimed,
// and nothing is claimed at all if any step fails (R7): a failed barrier acks
// nothing and leaves the stored runs wholly intact, because RunStore.Commit is
// atomic and is the last thing that can fail before the ack.
//
// The barrier hands Commit the ARTICLES the drain reported and does not group
// them. Deciding which of them form a run is derived state and it has exactly
// one owner, the store — which is also the only place the dedup against an
// at-least-once redelivery can be correct, because it has to subtract already
// stored art_idx values BEFORE grouping. See RunStore.Commit and the design
// doc's §6 for the worked example that ordering prevents.
//
// Run does not schedule itself. The cadence in R6 — a time bound, a byte
// bound, file completion, pause, and clean shutdown — belongs to the caller,
// so that "when to checkpoint" is a policy question and "what a checkpoint
// means" is this function.
//
// A storage fault never marks an article failed (A1). Retryable stalls the
// job, permanent fails it, and in both cases the articles stay Outstanding
// to be re-fetched.
func (b *Barrier) Run(ctx context.Context, jobID string, t SyncTarget) ([]PostAnomaly, error) {
	files := t.Files()
	drained := make(map[int32][]WrittenArticle, len(files))

	// Phase 1 — drain every file. Still no claim of any kind.
	//
	// A file that has left the open set since Files() listed it is dropped
	// from this run rather than treated as a failure of it: the close was
	// deliberate and drained and synced first, so there is nothing here to
	// checkpoint and nothing to surface (see ErrFileNotOpen).
	open := files[:0:0]
	for _, idx := range files {
		w, err := t.Drain(ctx, idx)
		if errors.Is(err, ErrFileNotOpen) {
			b.log.Debug("file closed under the barrier; dropped from this run",
				"job", jobID, "file", idx)
			continue
		}
		if err != nil {
			return nil, b.raise(jobID, "write", t.Path(idx), err)
		}
		open = append(open, idx)
		drained[idx] = w
	}
	files = open

	// Phase 2 — fsync every file. Only after this may anything be claimed.
	// Every file is synced before any file's articles are handed over, so a
	// barrier that fails on the second file's sync has claimed nothing about
	// the first either.
	//
	// A file dropped here leaves BOTH collections. Dropping it from drained
	// alone left it in files, so phase 3 went on to Stat it, got the same
	// ErrFileNotOpen back one phase later, and — because nothing there
	// recognised the sentinel — classified it as a storage fault. A healthy
	// job was parked, and the entire checkpoint was discarded including the
	// files that had already drained and fsynced successfully.
	//
	// Phase 3 now recognises the sentinel itself, so THIS line is defence in
	// depth rather than the fix, and no test reddens when it alone is removed
	// — verified by mutation, not assumed. It stays because carrying a file
	// the phase deliberately dropped is a latent inconsistency, and because
	// falling through to phase 3 relies on the target answering Stat with the
	// sentinel too, which is a property of one implementation rather than of
	// the interface.
	synced := files[:0:0]
	for _, idx := range files {
		if err := t.Sync(ctx, idx); err != nil {
			if errors.Is(err, ErrFileNotOpen) {
				// Raced with a close between phases. The drain report it
				// produced is still held by the writer and re-reported to
				// whoever opens the file next, so dropping it here loses
				// nothing (R12).
				b.log.Debug("file closed between drain and sync; dropped from this run",
					"job", jobID, "file", idx)
				delete(drained, idx)
				continue
			}
			return nil, b.raise(jobID, "sync", t.Path(idx), err)
		}
		synced = append(synced, idx)
	}
	files = synced

	// Phase 3 — collect what the fsync just made durable, and the size each
	// file now has.
	//
	// A file dropped by any phase leaves `files` there and then, and that is
	// what makes the drop complete rather than partial. The slice is also what
	// confirmAll releases reports over, so a file left in it after its report
	// was discarded has that report CONFIRMED — released from the writer with
	// nothing committed and nothing acked, destroying the re-report R12 relies
	// on to make the next drain whole.
	var arts []DurableArticle
	var acked []int32
	sizes := make(map[int32]int64, len(files))
	built := files[:0:0]
	for _, idx := range files {
		size, err := t.Stat(idx)
		if errors.Is(err, ErrFileNotOpen) {
			// Closed between the fsync and the stat — the same race phases 1
			// and 2 each handle, arriving at the third place a closed handle
			// can be noticed. Its bytes are on disk and its report is held by
			// the writer, so the file is dropped from this run rather than
			// failing it.
			b.log.Debug("file closed before it could be stat'ed; dropped from this run",
				"job", jobID, "file", idx)
			delete(drained, idx)
			continue
		}
		if err != nil {
			return nil, b.raise(jobID, "stat", t.Path(idx), err)
		}
		sizes[idx] = size
		for _, w := range drained[idx] {
			arts = append(arts, durableArticle(idx, w))
			acked = append(acked, w.ArtIdx)
		}
		built = append(built, idx)
	}
	files = built

	// Phase 4 — commit the runs atomically, then and only then ack. Nothing
	// between these two statements may fail, and nothing may be inserted
	// between them: the commit is what makes the proof true after a crash.
	if err := b.runs.Commit(ctx, jobID, arts); err != nil {
		return nil, fmt.Errorf("durability: barrier commit for %s: %w", jobID, err)
	}
	if len(acked) > 0 {
		slices.Sort(acked)
		// One of the two calls to newProof in the program; FinalizeFile has the
		// other. Both are on Barrier and both sit below a Sync that returned nil.
		// See the Barrier type doc.
		if err := b.ack.AckDurable(newProof(jobID, acked)); err != nil {
			return nil, fmt.Errorf("durability: barrier ack for %s: %w", jobID, err)
		}
	}
	// Only here, below both the commit and the ack. Releasing on the fsync —
	// which is what Sync used to do — left every failure between the two
	// unable to re-report, so a retried barrier had nothing to retry with.
	//
	// It runs even when nothing was acked: the commit landed, so the reports
	// those articles came from have done their work and must be released too —
	// otherwise a job whose files are all already durable re-reports them on
	// every checkpoint forever.
	b.confirmAll(ctx, files, t)
	b.log.Debug("durability barrier committed",
		"job", jobID, "files", len(files), "articles_acked", len(acked))

	// Findings ride the paths that COMMITTED, and every one of them — which is
	// not the same as "the paths that returned a nil error", and the
	// difference is a real defect this comment's first draft allowed. An
	// overlap is a property of the persisted runs, not of this cycle, so
	// withholding it from a checkpoint that failed loses nothing: the next
	// committing one re-derives it from the same rows. Reporting it from a
	// cycle that committed nothing would tell the user about a state the
	// barrier had just declined to make true.
	//
	// The acked==0 case is exactly the path a RESUMED job takes for a file
	// that became durable in an earlier process, where no later cycle
	// re-derives anything because the file never drains again — so it carries
	// the finding too, which is why this sits below the shared confirmAll
	// rather than inside a branch.
	//
	// Classified from the stored rows AFTER the commit, so this cycle's own
	// articles are included. It is deliberately not squeezed between the
	// commit and the ack, where §6 forbids anything at all.
	return b.admit(jobID, b.overlapFindings(ctx, jobID, files, sizes, t)), nil
}

// durableArticle converts one drained article into the record the store places.
//
// The whole conversion, in one place, so neither commit site can invent a
// different mapping. WrittenArticle already carries every field a run needs —
// see its doc for why the vocabularies mirror each other deliberately.
func durableArticle(fileIdx int32, w WrittenArticle) DurableArticle {
	return DurableArticle{
		FileIdx: fileIdx,
		ArtIdx:  w.ArtIdx,
		Offset:  w.Offset,
		Length:  w.Length,
		CRC32:   w.CRC32,
	}
}

// overlapFindings classifies each file's stored runs against the size the file
// now has, per §3.3: Σ Length greater than the file means articles wrote over
// each other.
//
// A read failure is logged and skipped rather than raised. The runs are on
// stable storage and the next committing checkpoint asks the same question of
// the same rows, so a failure here costs a delayed warning about a file that
// is repairable anyway — it must not fail a barrier whose commit and ack have
// already landed.
func (b *Barrier) overlapFindings(ctx context.Context, jobID string, files []int32, sizes map[int32]int64, t SyncTarget) []PostAnomaly {
	var found []PostAnomaly
	for _, idx := range files {
		runs, err := b.runs.ForFile(ctx, jobID, idx)
		if err != nil {
			b.log.Warn("could not read a file's durable runs to check it for overlaps; "+
				"the next checkpoint asks the same question of the same rows",
				"job", jobID, "file", idx, "err", err)
			continue
		}
		if pa, ok := overlapFrom(runs, sizes[idx], idx, func() string { return t.Path(idx) }); ok {
			found = append(found, pa)
		}
	}
	return found
}

// confirmAll releases every file's drain report once the cycle has landed.
//
// Confirm cannot fail, so this cannot either: the work it records is already
// done, and a report that is not released costs one redundant re-report that
// R12 requires the apply to absorb.
func (b *Barrier) confirmAll(ctx context.Context, files []int32, t SyncTarget) {
	for _, idx := range files {
		t.Confirm(ctx, idx)
	}
}

// routeFault dispatches a storage fault per A1 and returns it as the error,
// so a caller that ignores Stallable still cannot mistake a fault for
// success. f must be non-nil; every call site builds it from a non-nil error
// via storagefault.Classify, which returns nil only for a nil error.
//
// Neither branch marks an article failed. That is the whole distinction A1
// draws: storage faults resolve against storage, article faults against the
// article, and conflating them is how a full disk gets recorded as damage.
//
// # A coupling any new fault site must honour
//
// The returned error carries ErrFaultRouted, which is how the application
// layer knows this function already dispatched the fault and must not park the
// job a second time for it.
//
// It used to carry nothing, and the caller inferred the same thing from the
// mere PRESENCE of a *storagefault.Fault in the chain. That inference held
// only while this function was the one thing that let a fault escape the
// barrier, and it is not: the SyncTarget boundary mints its own fault when the
// worker does not answer, filewriter.go and assembler.go both build faults
// with storagefault.Classify, and any of those reaching the application layer
// was read as "already handled" and silently swallowed — the job carrying on
// with a file that was never trimmed.
//
// So the marker states the fact rather than leaving it to be deduced. A new
// fault site inside this package still routes through here; one that does not
// is now visibly unrouted rather than indistinguishable from a routed one.
// raise turns one SyncTarget error into the right kind of failure, and it is
// the single boundary every fault site in this file goes through.
//
// Three outcomes, and the middle one is the one that kept being missed:
//
//   - The target already classified it. Route the fault it built, keeping its
//     own op and path rather than relabelling them after the fact.
//   - It is not about storage at all — a deliberate close, an ordinary
//     shutdown's cancelled context, a stopped assembler. Return it unchanged
//     and route NOTHING. storagefault.Classify defaults everything it does not
//     recognise to retryable, so classifying one of these parks a healthy job
//     naming a disk that did not fail, with a reason no operator action clears.
//   - Anything else is a storage error the target reported raw. Classify and
//     route it, which is what this function used to do unconditionally.
//
// The sentinel set lives in SyncTarget's contract (ErrFileNotOpen,
// ErrTargetUnavailable) rather than here, because it is what an implementation
// promises the barrier — see ErrTargetUnavailable for why the promise is a
// boundary rule and not a list of call sites to patch.
func (b *Barrier) raise(jobID, op, path string, err error) error {
	if err == nil {
		return nil
	}
	if f, ok := errors.AsType[*storagefault.Fault](err); ok {
		// The target names the operation; only the path may be missing. A
		// target that raises a fault from its own timeout cannot resolve the
		// path itself — resolving it means calling back into the very
		// component that just failed to answer — so the barrier, which
		// already has it, fills it in. R27: a stall reason without a path is
		// one the operator cannot act on.
		if f.Path == "" {
			f.Path = path
		}
		return b.routeFault(jobID, f)
	}
	if errors.Is(err, ErrFileNotOpen) || errors.Is(err, ErrTargetUnavailable) {
		b.log.Info("barrier operation did not run, for a reason that is not a storage condition; "+
			"the job is not parked for it",
			"job", jobID, "op", op, "path", path, "err", err)
		return fmt.Errorf("durability: barrier %s job=%s: %w", op, jobID, err)
	}
	return b.routeFault(jobID, storagefault.Classify(op, path, err))
}

func (b *Barrier) routeFault(jobID string, f *storagefault.Fault) error {
	if f.Permanent {
		b.log.Error("durability barrier hit a permanent storage fault", "job", jobID, "fault", f)
		b.stall.Fail(jobID, f)
		return fmt.Errorf("%w: %w", ErrFaultRouted, f)
	}
	b.log.Warn("durability barrier hit a retryable storage fault", "job", jobID, "fault", f)
	b.stall.Stall(jobID, f)
	return fmt.Errorf("%w: %w", ErrFaultRouted, f)
}

// Truncator is a SyncTarget that can also trim a completed file.
//
// It is separate from SyncTarget because trimming is not part of a checkpoint:
// a barrier runs on a cadence over files that are still being written, and
// truncating one of those would destroy the bytes of every article that has
// not arrived yet. Only FinalizeFile trims, and only for a file whose parts
// have all been delivered.
type Truncator interface {
	SyncTarget
	Truncate(ctx context.Context, fileIdx int32, bound int64) error
}

// FinalizeFile checkpoints one completed file and trims it to its real extent.
//
// The bound is max(Offset+Length) over everything the record will hold once
// this call commits — the file's stored runs, plus the articles this drain
// just made durable — and getting that quantity right is the whole point of
// this function.
//
// It is emphatically NOT this run's high-water mark. That figure describes the
// articles this process happened to fetch, so on a resumed file it sits below
// what earlier runs wrote and truncating to it discards them. That is #342 and
// #350, and it is silent without par2.
//
// It is also NOT a gapless prefix. A gapless prefix deliberately stalls at the
// first hole, so a 40 GB file with one permanently failed article at 2 GB would
// be cut to 2 GB — far worse than the bug above, and it destroys precisely the
// blocks par2 would repair from. A run record has no trouble spanning a hole:
// the hole is simply a gap between two rows, and the maximum is taken over
// both.
//
// # Two guards deleted here on purpose
//
// This function used to carry two of them, and both existed only because the
// two records could disagree — one naming an article the other did not call
// durable, and the reverse. Neither state is representable in a single record
// written after the fsync, so both guards are gone rather than ported. Read
// their absence as the class being deleted, not as an oversight.
//
// A third guard went the same way, one layer up: the barrier used to refuse a
// target that reported zero articles, because committing a zero-width durable
// bitmap over the stored one erased every bit a job had accumulated. Nothing
// sizes a bitmap any more, so there is no zero-width bitmap and no erasure to
// guard against — SyncTarget.ArticleCount went with it.
//
// Truncation only ever shrinks (S6). The writer refuses a bound above the file
// on disk rather than clamping, because growing appends zeros, which asserts
// content that exists nowhere.
func (b *Barrier) FinalizeFile(ctx context.Context, jobID string, idx int32, t Truncator) ([]PostAnomaly, error) {
	written, err := t.Drain(ctx, idx)
	if errors.Is(err, ErrFileNotOpen) {
		// Some other path closed it first — a cancel, or CloseJobHandles on a
		// job entering post-processing — and both drained and synced before
		// closing. The caller checks the open set before reaching here, so
		// this is the narrow race rather than the ordinary case, and it is
		// still not a fault.
		b.log.Debug("file closed before its finalize could run", "job", jobID, "file", idx)
		return nil, nil
	}
	if err != nil {
		return nil, b.raise(jobID, "write", t.Path(idx), err)
	}
	if err := t.Sync(ctx, idx); err != nil {
		if errors.Is(err, ErrFileNotOpen) {
			// Closed between this function's own Drain and its Sync. The
			// Drain above already treats that close as "nothing left to do
			// here"; a close one statement later means the same thing, and
			// classifying it instead parked the job on a healthy disk, kept
			// an fd for a file the assembler had closed, and left the retry
			// re-running against a handle that cannot come back.
			b.log.Debug("file closed between its finalize drain and sync",
				"job", jobID, "file", idx)
			return nil, nil
		}
		return nil, b.raise(jobID, "sync", t.Path(idx), err)
	}

	arts := make([]DurableArticle, 0, len(written))
	acked := make([]int32, 0, len(written))
	for _, w := range written {
		arts = append(arts, durableArticle(idx, w))
		acked = append(acked, w.ArtIdx)
	}

	// The bound is taken BEFORE the commit and over both sources, rather than
	// after it over one. The commit has to be the last thing that can fail
	// before the ack (§6), so a truncate placed after it would sit between the
	// two statements nothing may be inserted between — and a truncate placed
	// after the ack would leave an untrimmed file behind on any crash, which
	// par2's QuickCheck reads as a missing file (§3.5) and works to
	// reconstruct.
	stored, err := b.runs.ForFile(ctx, jobID, idx)
	if err != nil {
		return nil, fmt.Errorf("durability: finalize runs job=%s file=%d: %w", jobID, idx, err)
	}
	bound := boundOver(stored, arts)

	if bound > 0 {
		if err := t.Truncate(ctx, idx, bound); err != nil {
			if errors.Is(err, ErrFileNotOpen) {
				b.log.Debug("file closed before its finalize could truncate",
					"job", jobID, "file", idx)
				return nil, nil
			}
			return nil, b.raise(jobID, "truncate", t.Path(idx), err)
		}
		// The truncate changed the file, so it is fsynced again before its
		// size is read: the size is what §3.3's overlap check and §3.4's
		// resume gate are both stated against, and reading it from a file
		// whose metadata is not yet on stable storage would compare against a
		// number the next restart does not see.
		if err := t.Sync(ctx, idx); err != nil {
			return nil, b.raise(jobID, "sync", t.Path(idx), err)
		}
	}
	// One stat, unconditionally — not nested inside `if bound > 0` above. A
	// bound of 0 means no run, stored or newly acked, claims any bytes, so the
	// truncate is skipped; nothing here is exempt from needing the file's real
	// size, though, because it is what §3.3's overlap check below is compared
	// against. Reading it from `t.Stat` in both branches — rather than falling
	// back to some zero value when bound == 0 — is what keeps that comparison
	// honest for a file this call finds already empty. After any truncate,
	// so the size reflects the trim when one happened. R27: a fault here
	// names the file, which is what makes the stall reason actionable.
	size, err := t.Stat(idx)
	if errors.Is(err, ErrFileNotOpen) {
		b.log.Debug("file closed before its finalize could stat it", "job", jobID, "file", idx)
		return nil, nil
	}
	if err != nil {
		return nil, b.raise(jobID, "stat", t.Path(idx), err)
	}

	if err := b.runs.Commit(ctx, jobID, arts); err != nil {
		return nil, fmt.Errorf("durability: finalize commit for %s file %d: %w", jobID, idx, err)
	}
	if len(acked) > 0 {
		slices.Sort(acked)
		if err := b.ack.AckDurable(newProof(jobID, acked)); err != nil {
			return nil, fmt.Errorf("durability: finalize ack for %s file %d: %w", jobID, idx, err)
		}
	}
	// Below both the commit and the ack, as in Run. A finalize that failed
	// between them used to lose the report as well, and the #342/#350 recovery
	// case is what that cost: with the report gone, the retry drains nothing
	// and a bound derived only from this run's articles sits below bytes that
	// are genuinely on disk.
	t.Confirm(ctx, idx)

	// A file being finalized has stopped receiving articles, so its overlap is
	// permanent and this is the last chance to notice it. Classified from the
	// stored rows after the commit, so the articles this call just recorded
	// are included, and against the POST-truncate size, which is the first
	// point at which the file's size is its true content length rather than
	// pre-allocation's.
	return b.admit(jobID, b.overlapFindings(ctx, jobID, []int32{idx}, map[int32]int64{idx: size}, t)), nil
}

// boundOver returns the highest end offset among a file's stored runs and the
// articles about to join them, or 0 when there are none.
//
// Both sources, because the commit that folds the second into the first has
// not happened yet — see FinalizeFile for why it cannot happen earlier. Taking
// the maximum rather than a sum is what lets the bound span a permanently
// failed article's hole instead of stopping at it.
func boundOver(stored []Run, arts []DurableArticle) int64 {
	var high int64
	for _, r := range stored {
		if end := r.Offset + r.Length; end > high {
			high = end
		}
	}
	for _, a := range arts {
		if end := a.Offset + int64(a.Length); end > high {
			high = end
		}
	}
	return high
}
