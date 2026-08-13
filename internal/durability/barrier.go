package durability

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// Barrier is the single place the Written → Durable → Resolved transition
// happens (X2). It is the only caller of newProof, so no other code path —
// inside this package or out — can ack an article as downloaded.
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
	for _, idx := range files {
		w, err := t.Drain(ctx, idx)
		if err != nil {
			return b.routeFault(jobID, storagefault.Classify("write", t.Path(idx), err))
		}
		drained[idx] = w
	}

	// Phase 2 — fsync every file. Only after this may anything be claimed.
	// Every file is synced before any file's extent is built, so a barrier
	// that fails on the second file's sync has claimed nothing about the
	// first either.
	for _, idx := range files {
		if err := t.Sync(ctx, idx); err != nil {
			return b.routeFault(jobID, storagefault.Classify("sync", t.Path(idx), err))
		}
	}

	// Phase 3 — build the Class B cache from what the fsync just made true.
	exts := make([]FileExtent, 0, len(files))
	var acked []int32
	for _, idx := range files {
		ext, arts, err := b.buildExtent(ctx, jobID, idx, drained[idx], t)
		if err != nil {
			return err
		}
		exts = append(exts, ext)
		acked = append(acked, arts...)
	}

	// Phase 4 — commit Class B atomically, then and only then ack. Nothing
	// between these two statements may fail, and nothing may be inserted
	// between them: the commit is what makes the proof true after a crash.
	if err := b.exts.Commit(ctx, jobID, exts); err != nil {
		return fmt.Errorf("durability: barrier commit for %s: %w", jobID, err)
	}
	if len(acked) == 0 {
		return nil
	}
	slices.Sort(acked)
	// The only call to newProof in the program. See the Barrier type doc.
	if err := b.ack.AckDurable(newProof(jobID, acked)); err != nil {
		return fmt.Errorf("durability: barrier ack for %s: %w", jobID, err)
	}
	b.log.Debug("durability barrier committed",
		"job", jobID, "files", len(exts), "articles_acked", len(acked))
	return nil
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
func (b *Barrier) buildExtent(ctx context.Context, jobID string, idx int32, drained []WrittenArticle, t SyncTarget) (FileExtent, []int32, error) {
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
		return FileExtent{}, nil, b.routeFault(jobID, storagefault.Classify("stat", t.Path(idx), err))
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

	verified, err := b.gaplessPrefix(ctx, jobID, idx, ext.Durable, t)
	if err != nil {
		return FileExtent{}, nil, err
	}
	if verified != ext.VerifiedTo {
		// PrefixCRC is defined as the CRC32 of [0, VerifiedTo), so carrying
		// the loaded one across a recomputed VerifiedTo would relabel the
		// CRC of one prefix as the CRC of a different one — design Shape 1
		// ("a fact recorded before the thing it asserts becomes true") in
		// the one file whose job is to prevent it. R23 already blesses
		// "unavailable" as an honest answer, and HasPrefixCRC is what makes
		// it distinguishable from a CRC of zero.
		//
		// The barrier itself never writes a PrefixCRC. Resumer does, off
		// this critical path, and it recomputes the CRC from the bytes it
		// reads rather than expecting the field to have survived a barrier
		// that moved the anchor — which is why clearing it here costs
		// nothing but a stale claim.
		ext.PrefixCRC = 0
		ext.HasPrefixCRC = false
	}
	ext.VerifiedTo = verified
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
// Returning the fault is how the application layer knows it has been handled.
// Application.routeFinalizeFailure treats a *storagefault.Fault anywhere in an
// error chain as proof that this function already dispatched it, and so leaves
// it alone — which is what stops a permanent fault from being relabelled a
// stall on its way out.
//
// That inference is only sound while THIS function is the only thing that lets
// a *Fault escape the barrier. It is not the only thing that mints one:
// filewriter.go and assembler.go both build faults with storagefault.Classify,
// and any of those reaching the application layer without having been routed
// would be silently swallowed — the job halting with a Debug line and no reason
// the operator can see.
//
// So a new fault site inside this package must either route through here, or
// not surface a *Fault to its caller at all. Wrapping one in a plain error is
// enough to make it invisible to the check and is the wrong direction; convert
// it, or route it.
func (b *Barrier) routeFault(jobID string, f *storagefault.Fault) error {
	if f.Permanent {
		b.log.Error("durability barrier hit a permanent storage fault", "job", jobID, "fault", f)
		b.stall.Fail(jobID, f)
		return f
	}
	b.log.Warn("durability barrier hit a retryable storage fault", "job", jobID, "fault", f)
	b.stall.Stall(jobID, f)
	return f
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
//     design forbids. priorExtent has artCount and Load does not, so this is
//     the only place the mask can be applied. Re-deriving through
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
	stored, err := b.exts.Load(ctx, jobID)
	if err != nil {
		return FileExtent{}, fmt.Errorf("durability: barrier load prior extent job=%s file=%d: %w", jobID, idx, err)
	}
	for _, e := range stored {
		if e.FileIdx != idx {
			continue
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
	return FileExtent{Durable: NewBitmap(artCount)}, nil
}

// gaplessPrefix returns the length of the durable, contiguous run of bytes
// starting at offset 0 — the CRC anchor VerifiedTo records.
//
// A hole stops this walk and leaves durable untouched. That separation is
// the #311/#353 distinction: resume reads the bitmap, so an article above
// the hole is still durable and is not re-fetched, while the CRC anchor
// reads this, so it does not claim a prefix it cannot prove. Collapsing the
// two into one value is what made writeCursor unusable as a durability
// anchor.
//
// FactLog.ForFile already returns facts ordered by Offset — a Task 4 test
// pins that — so the walk needs no sort of its own.
//
// A read failure is returned rather than degraded to 0: an unreadable fact
// log yields no evidence, and committing VerifiedTo = 0 as if it were
// derived would be a silent wrong answer, which A2 and R28 forbid.
func (b *Barrier) gaplessPrefix(ctx context.Context, jobID string, idx int32, durable Bitmap, t SyncTarget) (int64, error) {
	facts, err := b.facts.ForFile(ctx, jobID, idx)
	if err != nil {
		return 0, fmt.Errorf("durability: barrier gapless prefix job=%s file=%d: %w", jobID, idx, err)
	}
	var prefix int64
	for _, f := range facts {
		// Not exactly abutting the run so far: either a hole, or an overlap
		// this walk cannot prove tiles the range (R23). Either way the
		// prefix ends here.
		if f.Offset != prefix {
			break
		}
		ord, ok := t.FileLocalOrdinal(idx, f.ArtIdx)
		if !ok || !durable.Get(ord) {
			break
		}
		prefix = f.Offset + int64(f.Length)
	}
	return prefix, nil
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
	if err != nil {
		return b.routeFault(jobID, storagefault.Classify("write", t.Path(idx), err))
	}
	if err := t.Sync(ctx, idx); err != nil {
		return b.routeFault(jobID, storagefault.Classify("sync", t.Path(idx), err))
	}
	ext, acked, err := b.buildExtent(ctx, jobID, idx, written, t)
	if err != nil {
		return err
	}

	bound, err := b.durableExtent(ctx, jobID, idx, ext.Durable, t)
	if err != nil {
		return err
	}
	// A completed file whose recorded articles are not ALL durable must not be
	// trimmed to the durable bound.
	//
	// In the healthy case the two sets coincide: every article of a completed
	// file is Done — hence durable — or permanently failed, and a failed
	// article never decoded and so wrote no Class A fact. A gap between them
	// means this is a RETRY of a finalize whose earlier attempt consumed the
	// writer's drain report without committing the bits it earned. Those bytes
	// are on disk; the durable bound sits below them; truncating to it destroys
	// them. That is the #342/#350 family arriving through the recovery path,
	// and it is silent.
	//
	// The recorded bound is safe where the durable one is not: every fact names
	// an article that decoded, so nothing below its end is pre-allocation
	// padding, and Truncate only ever shrinks (S6). Trailing zeros par2 reports
	// as damage is a visible, repairable cost; downloaded bytes gone is not.
	if bound > 0 {
		recorded, missing, err := b.recordedExtent(ctx, jobID, idx, ext.Durable, t)
		if err != nil {
			return err
		}
		if missing > 0 {
			b.log.Warn("finalizing a file whose recorded articles are not all durable; "+
				"trimming to the recorded extent rather than the durable one so an "+
				"interrupted finalize's bytes are not destroyed",
				"job", jobID, "file", idx, "not_durable", missing,
				"durable_extent", bound, "recorded_extent", recorded)
			bound = recorded
		}
	}
	if bound > 0 {
		if err := t.Truncate(ctx, idx, bound); err != nil {
			return b.routeFault(jobID, storagefault.Classify("truncate", t.Path(idx), err))
		}
		// The truncate changed the file, so the size/mtime stamp taken inside
		// buildExtent describes a file that no longer exists. Re-stat and
		// re-sync: a stamp that does not match the file on disk fails S7's
		// validity check on the next resume and throws away a valid cache.
		if err := t.Sync(ctx, idx); err != nil {
			return b.routeFault(jobID, storagefault.Classify("sync", t.Path(idx), err))
		}
		size, modNs, err := t.Stat(idx)
		if err != nil {
			return b.routeFault(jobID, storagefault.Classify("stat", "", err))
		}
		ext.Size, ext.ModTimeNs = size, modNs
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
	return nil
}

// durableExtent returns the highest end offset among the file's durable
// articles, or 0 when none is durable.
//
// Unlike gaplessPrefix this does NOT stop at a hole. A hole means an article
// permanently failed, and the bytes above it are still real bytes that par2
// repairs from — stopping there is the data-loss bug this function exists to
// avoid. The two walks read the same facts and answer deliberately different
// questions: gaplessPrefix asks "what can I prove about a CRC anchor", this
// asks "how long is the file".
func (b *Barrier) durableExtent(ctx context.Context, jobID string, idx int32, durable Bitmap, t SyncTarget) (int64, error) {
	facts, err := b.facts.ForFile(ctx, jobID, idx)
	if err != nil {
		return 0, fmt.Errorf("durability: durable extent job=%s file=%d: %w", jobID, idx, err)
	}
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
	return high, nil
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
func (b *Barrier) recordedExtent(ctx context.Context, jobID string, idx int32, durable Bitmap, t SyncTarget) (high int64, missing int, err error) {
	facts, err := b.facts.ForFile(ctx, jobID, idx)
	if err != nil {
		return 0, 0, fmt.Errorf("durability: recorded extent job=%s file=%d: %w", jobID, idx, err)
	}
	for _, f := range facts {
		if end := f.Offset + int64(f.Length); end > high {
			high = end
		}
		if ord, ok := t.FileLocalOrdinal(idx, f.ArtIdx); !ok || !durable.Get(ord) {
			missing++
		}
	}
	return high, missing, nil
}
