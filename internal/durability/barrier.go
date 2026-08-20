package durability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// Barrier is the single place the Written → Durable → Resolved transition
// happens (X2). Its two methods Run and FinalizeFile are the only callers of
// newProof, so no other code path — inside this package or out — can ack an
// article THROUGH Queue.AckDurable.
//
// That is narrower than "can ack an article as downloaded", which is what this
// comment used to say and is false. Queue.SeedFromExtents and
// Queue.ReplaceFromResume also reach markDone, take a fully exported
// durability.FileExtent whose Bitmap is exported and settable, and are
// therefore callable from any package with no barrier and no proof. That is by
// DESIGN — their evidence is stable storage re-read at startup, not an fsync
// this process performed, which is the one kind of evidence a proof cannot
// represent — but it means the proof gates the barrier door only. The seeding
// doors are held by their own contracts and tests, not by the compiler.
//
// Before this design the assembler could ack from six places, and the same
// defect was refiled twice (#355, #356). One place is the whole point;
// adding a second caller of newProof silently undoes it.
type Barrier struct {
	facts FactLog
	exts  ExtentStore
	ack   Acker
	stall Stallable
	log   *slog.Logger
}

// NewBarrier wires a barrier. It owns none of its collaborators' lifecycles,
// and holds no lock of its own: Run does I/O throughout, and the project
// bans I/O under a lock.
func NewBarrier(fl FactLog, es ExtentStore, ack Acker, stall Stallable, log *slog.Logger) *Barrier {
	return &Barrier{facts: fl, exts: es, ack: ack, stall: stall, log: log}
}

// Run executes one checkpoint for a job:
//
//	drain → fsync → commit Class B → ack
//
// The order is the invariant (S1). Nothing before the fsync may be claimed,
// and nothing is claimed at all if any step fails (R7): a failed barrier acks
// nothing and leaves the previously committed cache wholly intact, because
// ExtentStore.Commit is atomic and is the last thing that can fail before the
// ack.
//
// Run does not schedule itself. The cadence in R6 — a time bound, a byte
// bound, file completion, pause, and clean shutdown — belongs to the caller,
// so that "when to checkpoint" is a policy question and "what a checkpoint
// means" is this function.
//
// A storage fault never marks an article failed (A1). Retryable stalls the
// job, permanent fails it, and in both cases the articles stay Outstanding
// to be re-fetched.
func (b *Barrier) Run(ctx context.Context, jobID string, t SyncTarget) error {
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
			return b.raise(jobID, "write", t.Path(idx), err)
		}
		open = append(open, idx)
		drained[idx] = w
	}
	files = open

	// Phase 2 — fsync every file. Only after this may anything be claimed.
	// Every file is synced before any file's extent is built, so a barrier
	// that fails on the second file's sync has claimed nothing about the
	// first either.
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
			return b.raise(jobID, "sync", t.Path(idx), err)
		}
		synced = append(synced, idx)
	}
	files = synced

	// Phase 3 — build the Class B cache from what the fsync just made true.
	//
	// A file dropped by any phase leaves `files` there and then, and that is
	// what makes the drop complete rather than partial. The slice is also what
	// confirmAll releases reports over, so a file left in it after its report
	// was discarded has that report CONFIRMED — released from the writer with
	// nothing committed and nothing acked, destroying the re-report R12 relies
	// on to make the next drain whole.
	exts := make([]FileExtent, 0, len(files))
	built := files[:0:0]
	var acked []int32
	for _, idx := range files {
		facts, err := b.facts.ForFile(ctx, jobID, idx)
		if err != nil {
			return fmt.Errorf("durability: barrier facts job=%s file=%d: %w", jobID, idx, err)
		}
		ext, arts, err := b.buildExtent(ctx, jobID, idx, drained[idx], facts, t)
		if errors.Is(err, ErrFileNotOpen) {
			// Closed between the fsync and the stat — the same race phases 1
			// and 2 each handle, arriving at the third place a closed handle
			// can be noticed. Its bytes are on disk and its report is held by
			// the writer, so the file is dropped from this run rather than
			// failing it.
			b.log.Debug("file closed before its extent could be built; dropped from this run",
				"job", jobID, "file", idx)
			delete(drained, idx)
			continue
		}
		if err != nil {
			return err
		}
		exts = append(exts, ext)
		acked = append(acked, arts...)
		built = append(built, idx)
	}
	files = built

	// Phase 4 — commit Class B atomically, then and only then ack. Nothing
	// between these two statements may fail, and nothing may be inserted
	// between them: the commit is what makes the proof true after a crash.
	if err := b.exts.Commit(ctx, jobID, exts); err != nil {
		return fmt.Errorf("durability: barrier commit for %s: %w", jobID, err)
	}
	if len(acked) == 0 {
		// Nothing was acked, but the commit landed, so the reports these
		// extents were built from have done their work and must be released
		// too — otherwise a job whose files are all already durable re-reports
		// them on every checkpoint forever.
		b.confirmAll(ctx, files, t)
		return nil
	}
	slices.Sort(acked)
	// One of the two calls to newProof in the program; FinalizeFile has the
	// other. Both are on Barrier and both sit below a Sync that returned nil.
	// See the Barrier type doc.
	if err := b.ack.AckDurable(newProof(jobID, acked)); err != nil {
		return fmt.Errorf("durability: barrier ack for %s: %w", jobID, err)
	}
	// Only here, below both the commit and the ack. Releasing on the fsync —
	// which is what Sync used to do — left every failure between the two
	// unable to re-report, so a retried barrier had nothing to retry with.
	b.confirmAll(ctx, files, t)
	b.log.Debug("durability barrier committed",
		"job", jobID, "files", len(exts), "articles_acked", len(acked))
	return nil
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

// buildExtent derives one file's Class B cache from the articles the fsync
// just made durable, and returns the article indices that may be acked.
//
// It claims nothing on its own: the extent it returns is not persisted and the
// articles it names are not acked until the caller commits.
//
// This is the chokepoint for every extent that can reach ExtentStore.Commit.
// Run's phase 3 and FinalizeFile are the only two callers, and Commit has no
// other call site, so a guard here covers every path that can overwrite a
// stored extent — which a guard on Files() did not, because FinalizeFile takes
// an explicit file index and never calls it.
func (b *Barrier) buildExtent(ctx context.Context, jobID string, idx int32, drained []WrittenArticle, facts []ArticleFact, t SyncTarget) (FileExtent, []int32, error) {
	// A target that cannot say how many articles the file holds must not have
	// an extent built for it, and this is a data-loss guard rather than
	// tidiness.
	//
	// priorExtent re-derives the stored bitmap at artCount bits. At zero, that
	// yields a zero-WIDTH bitmap, and committing it replaces the stored one —
	// erasing every durable bit the job had accumulated. The next restart then
	// reads nothing as durable and re-downloads the whole job, which is exactly
	// the ground L3 says a restart must not lose.
	//
	// A drain with at least one article happens to abort later on the
	// FileLocalOrdinal lookup, so the erasure needs an EMPTY drain to be
	// reachable — a checkpoint over a file that took no writes this interval,
	// which is an ordinary event rather than an exotic one.
	//
	// Refusing loudly rather than skipping the file follows A2 and matches how
	// the ordinal lookup below is already treated: an unusable article count is
	// a bookkeeping defect, not a storage fault, so it does not go through
	// Stallable — blaming storage for it would be the A1 conflation in reverse.
	artCount := t.ArticleCount(idx)
	if artCount <= 0 {
		return FileExtent{}, nil, fmt.Errorf(
			"durability: job %s file %d: target reports %d articles, refusing to build an extent "+
				"(committing a zero-width bitmap would erase the stored durable bits)",
			jobID, idx, artCount)
	}
	size, modNs, err := t.Stat(idx)
	if err != nil {
		return FileExtent{}, nil, b.raise(jobID, "stat", t.Path(idx), err)
	}
	ext, err := b.priorExtent(ctx, jobID, idx, artCount)
	if err != nil {
		return FileExtent{}, nil, err
	}
	ext.FileIdx = idx
	ext.Size = size
	ext.ModTimeNs = modNs

	var acked []int32
	for _, w := range drained {
		ord, ok := t.FileLocalOrdinal(idx, w.ArtIdx)
		if !ok {
			// The target could not place this article in its file. That is
			// a bookkeeping defect, not a storage fault, and it must not be
			// swallowed (A2, R28). Routing it through Stallable would also
			// be wrong: it would blame storage for a numbering bug.
			return FileExtent{}, nil, fmt.Errorf("durability: job %s file %d: article %d has no file-local ordinal", jobID, idx, w.ArtIdx)
		}
		// Charge the bytes only on a 0->1 transition. Drain may report an
		// article this or a previous barrier already recorded — R12 makes
		// at-least-once delivery the contract and requires the apply to
		// absorb it — and += outside this guard would inflate the R26
		// "bytes durable" figure on every replay. Set() is idempotent; the
		// accumulator is not.
		if !ext.Durable.Get(ord) {
			ext.Durable.Set(ord)
			ext.BytesDurable += int64(w.Length)
		}
		acked = append(acked, w.ArtIdx)
	}

	// The barrier does NOT write ArticleFacts. Class A is appended by the
	// writer when the article is decoded, with no ordering against the data
	// (R2) — that independence is what lets Class A be committed without a
	// barrier at all. Writing facts here would make them barrier-ordered and
	// quietly destroy the property.

	verified, prefixCRC, whole := verifiedPrefix(facts, durableAt(facts, idx, ext.Durable, t), size)
	// Both assigned from the same walk, so the CRC always describes exactly
	// the prefix beside it. The old code could only CLEAR them — it carried a
	// loaded PrefixCRC across an unchanged VerifiedTo and zeroed it otherwise,
	// and nothing on this path ever produced one. That left a file downloaded
	// without a restart with no CRC at any point in its life, because Resumer
	// was the only writer and it runs at startup.
	//
	// Relabelling one prefix's CRC as another's is still the hazard it was;
	// recomputing both together is what makes it unreachable, rather than a
	// rule about when to discard.
	ext.VerifiedTo = verified
	ext.PrefixCRC = prefixCRC
	// A prefix that reaches the file's end IS the whole file — but only when
	// it also accounts for every fact, which is why verifiedPrefix decides it
	// rather than this call site. Deriving it here from VerifiedTo and Size
	// alone is what let an overlapping article's leftover fact go unnoticed
	// (#387). During a download this is normally false — the stat reads
	// pre-allocation's full size while the prefix is still growing — and
	// FinalizeFile re-evaluates it after the truncate, where size is the
	// file's true end. R23 keeps "unavailable" distinguishable from a CRC of
	// zero.
	ext.HasPrefixCRC = whole
	return ext, acked, nil
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

// priorExtent loads the committed extent for one file, or returns a zero
// extent with an empty artCount-wide bitmap when none exists.
//
// The bitmap is re-derived at artCount bits rather than adopted as loaded,
// and that is load-bearing in both directions:
//
//   - ExtentStore.Load rebuilds each bitmap at its full BYTE width, because
//     the blob is all it has. That width is always a multiple of 64, so
//     Bitmap's tail-word mask cannot fire, and any padding bits in a damaged
//     or externally-written blob survive into Count() — over-reporting how
//     many articles are durable, which is the over-claim direction the
//     design forbids. priorExtent has artCount and the store does not, so
//     this is the only place the mask can be applied. Re-deriving through
//     BitmapFromBytes applies it; narrowing what Load returned would not.
//
//   - A stored bitmap narrower than artCount is widened by zero-padding,
//     which reads as "those articles are not durable yet" — the safe
//     direction under S3.
//
// The extent is returned by value and Bitmap.Set has a value receiver that
// mutates through the shared backing slice, so the caller's ext.Durable.Set
// reaches this bitmap's storage. That holds only while Set never reassigns
// words; see the note on Bitmap.Set.
func (b *Barrier) priorExtent(ctx context.Context, jobID string, idx int32, artCount int) (FileExtent, error) {
	e, ok, err := b.exts.LoadFile(ctx, jobID, idx)
	if err != nil {
		return FileExtent{}, fmt.Errorf("durability: barrier load prior extent job=%s file=%d: %w", jobID, idx, err)
	}
	if !ok {
		return FileExtent{Durable: NewBitmap(artCount)}, nil
	}
	raw := e.Durable.Bytes()
	if need := (artCount + 63) / 64 * 8; len(raw) < need {
		widened := make([]byte, need)
		copy(widened, raw)
		raw = widened
	}
	bm, err := BitmapFromBytes(raw, artCount)
	if err != nil {
		return FileExtent{}, fmt.Errorf("durability: barrier re-derive bitmap job=%s file=%d: %w", jobID, idx, err)
	}
	e.Durable = bm
	return e, nil
}

// durableAt builds verifiedPrefix's per-fact predicate for the barrier: a
// fact counts when its article has a file-local ordinal and that ordinal's
// durable bit is set.
//
// A hole stops the walk and leaves durable untouched. That separation is the
// #311/#353 distinction: resume reads the bitmap, so an article above the hole
// is still durable and is not re-fetched, while the CRC anchor reads the walk,
// so it does not claim a prefix it cannot prove. Collapsing the two into one
// value is what made writeCursor unusable as a durability anchor.
//
// A closure rather than a precomputed []bool so the ordinal lookup is skipped
// for every fact past the break — verifiedPrefix stops at the first false.
func durableAt(facts []ArticleFact, idx int32, durable Bitmap, t SyncTarget) func(i int) bool {
	return func(i int) bool {
		ord, ok := t.FileLocalOrdinal(idx, facts[i].ArtIdx)
		return ok && durable.Get(ord)
	}
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
// The bound is the highest end offset among the file's DURABLE articles —
// max(Offset+Length) over the facts whose durable bit is set — and getting
// that quantity right is the whole point of this function.
//
// It is emphatically NOT this run's high-water mark. That figure describes the
// articles this process happened to fetch, so on a resumed file it sits below
// what earlier runs wrote and truncating to it discards them. That is #342 and
// #350, and it is silent without par2.
//
// It is also NOT FileExtent.VerifiedTo. VerifiedTo is the GAPLESS PREFIX and
// deliberately stalls at the first hole, so a 40 GB file with one permanently
// failed article at 2 GB would be cut to 2 GB — far worse than the bug above,
// and it destroys precisely the blocks par2 would repair from. Extent and
// gapless prefix are different quantities and the distinction is load-bearing.
//
// Deriving it from Class A rather than storing it means there is no second
// copy to drift (S5), and it is correct by construction: every fact counted
// has an article behind it whose bytes a completed fsync covered.
//
// Truncation only ever shrinks (S6). The writer refuses a bound above the file
// on disk rather than clamping, because growing appends zeros, which asserts
// content that exists nowhere.
func (b *Barrier) FinalizeFile(ctx context.Context, jobID string, idx int32, t Truncator) error {
	written, err := t.Drain(ctx, idx)
	if errors.Is(err, ErrFileNotOpen) {
		// Some other path closed it first — a cancel, or CloseJobHandles on a
		// job entering post-processing — and both drained and synced before
		// closing. The caller checks the open set before reaching here, so
		// this is the narrow race rather than the ordinary case, and it is
		// still not a fault.
		b.log.Debug("file closed before its finalize could run", "job", jobID, "file", idx)
		return nil
	}
	if err != nil {
		return b.raise(jobID, "write", t.Path(idx), err)
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
			return nil
		}
		return b.raise(jobID, "sync", t.Path(idx), err)
	}
	// One read of the file's facts for all three walks below. They ask three
	// different questions of the same rows — the CRC anchor, the durable
	// bound, the recorded bound — and each used to load them again.
	facts, err := b.facts.ForFile(ctx, jobID, idx)
	if err != nil {
		return fmt.Errorf("durability: finalize facts job=%s file=%d: %w", jobID, idx, err)
	}
	ext, acked, err := b.buildExtent(ctx, jobID, idx, written, facts, t)
	if errors.Is(err, ErrFileNotOpen) {
		b.log.Debug("file closed before its finalize could stat it", "job", jobID, "file", idx)
		return nil
	}
	if err != nil {
		return err
	}

	bound := durableExtent(facts, idx, ext.Durable, t)
	// A completed file whose recorded articles are not ALL durable must not be
	// trimmed to the durable bound.
	//
	// The two sets coincide only when every article of a completed file is
	// Done — hence durable — or was permanently failed WITHOUT decoding, which
	// is the only kind that wrote no Class A fact.
	//
	// An earlier version of this paragraph asserted that of every failed
	// article. It is false for the two the assembler refuses after decoding:
	// an arrival at an already-written offset, and an article displaced from
	// one. Both decoded, so both have a Class A fact appended at decode time
	// and independent of any write, and neither earns a durable bit. #386 made
	// the displaced case routinely reachable — before it, a file with a
	// cache-resident collision never reached TotalParts and so never finalized
	// at all. See #389, which is about what that costs rather than what it is.
	//
	// So a gap between the sets means one of two things: a collision of that
	// kind, or a RETRY of a finalize whose earlier attempt consumed the
	// writer's drain report without committing the bits it earned. The second
	// is why the bound must not drop: those bytes are on disk, the durable
	// bound sits below them, and truncating to it destroys them. That is the
	// #342/#350 family arriving through the recovery path, and it is silent.
	//
	// The recorded bound is safe where the durable one is not: every fact names
	// an article that decoded, so nothing below its end is pre-allocation
	// padding, and Truncate only ever shrinks (S6). Trailing zeros par2 reports
	// as damage is a visible, repairable cost; downloaded bytes gone is not.
	if bound > 0 {
		recorded, missing, unrecorded := recordedExtent(facts, idx, ext.Durable, t)
		if missing > 0 {
			b.log.Warn("finalizing a file whose recorded articles are not all durable; "+
				"trimming to the recorded extent rather than the durable one so no "+
				"already-written bytes are destroyed. Expected after an offset "+
				"collision, or on a retry of an interrupted finalize",
				"job", jobID, "file", idx, "not_durable", missing,
				"durable_extent", bound, "recorded_extent", recorded)
			bound = recorded
		}
		// The mirror case, and the one no fact-derived bound can survive: an
		// article that is durable but that Class A does not name.
		//
		// It is reachable because R2 makes the fact append independent of the
		// write — pipeline.appendArticleFacts logs its error and lets the
		// write proceed — so an article can reach disk, be drained, be
		// fsynced, and earn a truthful durable bit while having no fact. Both
		// durableExtent and recordedExtent answer by walking facts, so both
		// walk straight past its bytes. When it holds the file's top offset
		// they return a bound below its end and the truncate destroys it,
		// while buildExtent has already added it to acked — so it is marked
		// Done in the same call and never re-fetched.
		//
		// Refuse to truncate at all rather than trying to repair the bound.
		// The bytes are on disk and correct; what is missing is the record
		// that would let a walk find them, and no bound derived from that
		// record can be trusted while it is incomplete. Leaving pre-allocation
		// zeros is the same visible, repairable cost the recorded-bound
		// fallback above already accepts (S6 only ever shrinks, so declining
		// to shrink is always safe).
		if unrecorded > 0 {
			b.log.Warn("finalizing a file with durable articles the fact log does not name; "+
				"skipping the truncate entirely, because every fact-derived bound sits "+
				"below their bytes and would destroy them",
				"job", jobID, "file", idx, "unrecorded", unrecorded,
				"durable_extent", bound, "recorded_extent", recorded)
			bound = 0
		}
	}
	if bound > 0 {
		if err := t.Truncate(ctx, idx, bound); err != nil {
			if errors.Is(err, ErrFileNotOpen) {
				b.log.Debug("file closed before its finalize could truncate",
					"job", jobID, "file", idx)
				return nil
			}
			return b.raise(jobID, "truncate", t.Path(idx), err)
		}
		// The truncate changed the file, so the size/mtime stamp taken inside
		// buildExtent describes a file that no longer exists. Re-stat and
		// re-sync: a stamp that does not match the file on disk fails S7's
		// validity check on the next resume and throws away a valid cache.
		if err := t.Sync(ctx, idx); err != nil {
			return b.raise(jobID, "sync", t.Path(idx), err)
		}
		size, modNs, err := t.Stat(idx)
		if err != nil {
			return b.raise(jobID, "stat", t.Path(idx), err)
		}
		ext.Size, ext.ModTimeNs = size, modNs
		// buildExtent judged this against pre-allocation's size, where a
		// complete file's prefix falls short of the stat and the CRC reads as
		// unavailable. The truncate has just made Size the file's true end, so
		// this is the first point at which "the prefix covers the whole file"
		// can be answered.
		//
		// Re-asked through verifiedPrefix against the same facts, NOT recomputed
		// from VerifiedTo and Size. Those two alone cannot see a fact the walk
		// never consumed, which is how an article overlapping a sibling without
		// extending the file published a CRC of the bytes that should have been
		// written (#387). The walk is cheap — arithmetic over rows already in
		// memory, no read of the file (R24).
		_, _, whole := verifiedPrefix(facts, durableAt(facts, idx, ext.Durable, t), ext.Size)
		ext.HasPrefixCRC = whole
	}

	if err := b.exts.Commit(ctx, jobID, []FileExtent{ext}); err != nil {
		return fmt.Errorf("durability: finalize commit for %s file %d: %w", jobID, idx, err)
	}
	if len(acked) == 0 {
		return nil
	}
	slices.Sort(acked)
	if err := b.ack.AckDurable(newProof(jobID, acked)); err != nil {
		return fmt.Errorf("durability: finalize ack for %s file %d: %w", jobID, idx, err)
	}
	// Below both the commit and the ack, as in Run. A finalize that failed
	// between them used to lose the report as well, and it is the retry of
	// THIS path that the #342/#350 recovery guard exists to serve — with the
	// report gone, the retry drains nothing and the durable bound sits below
	// bytes that are genuinely on disk.
	t.Confirm(ctx, idx)
	return nil
}

// durableExtent returns the highest end offset among the file's durable
// articles, or 0 when none is durable.
//
// Unlike verifiedPrefix this does NOT stop at a hole. A hole means an article
// permanently failed, and the bytes above it are still real bytes that par2
// repairs from — stopping there is the data-loss bug this function exists to
// avoid. The two walks read the same facts and answer deliberately different
// questions: verifiedPrefix asks "what can I prove about a CRC anchor", this
// asks "how long is the file".
func durableExtent(facts []ArticleFact, idx int32, durable Bitmap, t SyncTarget) int64 {
	var high int64
	for _, f := range facts {
		ord, ok := t.FileLocalOrdinal(idx, f.ArtIdx)
		if !ok || !durable.Get(ord) {
			continue
		}
		if end := f.Offset + int64(f.Length); end > high {
			high = end
		}
	}
	return high
}

// recordedExtent returns the highest end offset among ALL of the file's
// recorded facts, and how many of them are not durable.
//
// It is the fallback bound FinalizeFile uses when the durable set is provably
// incomplete — see the guard there for why a durable-only bound is unsafe on a
// retried finalize. The count is what decides that, so it is returned rather
// than derived again by the caller: "some fact has no durable bit" and "the
// recorded extent is longer than the durable one" are not the same condition,
// and a non-durable article at a LOW offset satisfies only the first.
//
// unrecorded is the OTHER direction: durable articles the fact log does not
// name at all. It is counted here because this walk already holds both sides,
// and it is separate from missing because the two have opposite remedies — a
// recorded-but-not-durable article makes the recorded bound the safe one,
// while a durable-but-unrecorded article makes EVERY fact-derived bound
// unsafe, since no walk over facts can see the bytes it holds.
func recordedExtent(facts []ArticleFact, idx int32, durable Bitmap, t SyncTarget) (high int64, missing, unrecorded int) {
	// Distinct rather than a plain counter: Class A is appended at least once
	// per article (R12), so a replayed append would otherwise cancel out a
	// genuinely unrecorded article and hide the very case this counts.
	covered := make(map[int]struct{}, len(facts))
	for _, f := range facts {
		if end := f.Offset + int64(f.Length); end > high {
			high = end
		}
		ord, ok := t.FileLocalOrdinal(idx, f.ArtIdx)
		if !ok || !durable.Get(ord) {
			missing++
			continue
		}
		covered[ord] = struct{}{}
	}
	return high, missing, durable.Count() - len(covered)
}
