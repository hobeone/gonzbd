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
			return b.routeFault(jobID, storagefault.Classify("write", "", err))
		}
		drained[idx] = w
	}

	// Phase 2 — fsync every file. Only after this may anything be claimed.
	// Every file is synced before any file's extent is built, so a barrier
	// that fails on the second file's sync has claimed nothing about the
	// first either.
	for _, idx := range files {
		if err := t.Sync(ctx, idx); err != nil {
			return b.routeFault(jobID, storagefault.Classify("sync", "", err))
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
// It is phase 3 of Run and claims nothing on its own: the extent it returns
// is not persisted and the articles it names are not acked until Run's
// phase 4 succeeds.
func (b *Barrier) buildExtent(ctx context.Context, jobID string, idx int32, drained []WrittenArticle, t SyncTarget) (FileExtent, []int32, error) {
	size, modNs, err := t.Stat(idx)
	if err != nil {
		return FileExtent{}, nil, b.routeFault(jobID, storagefault.Classify("stat", "", err))
	}
	ext, err := b.priorExtent(ctx, jobID, idx, t.ArticleCount(idx))
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
