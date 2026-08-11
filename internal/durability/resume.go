package durability

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"log/slog"
	"os"

	"github.com/hobeone/gonzbd/internal/crc32util"
)

// ErrArticleOutOfRange reports a recorded fact whose article index cannot be a
// bit position in a file of the given article count. See Resumer's type doc
// for why Resume has to make that identification at all.
var ErrArticleOutOfRange = errors.New("durability: article index outside the file's article count")

// ResumeResult is what one file's resume established from stable storage.
//
// Durable is indexed like FileExtent.Durable and is the only field resume
// decisions may read: an article whose bit is clear is Outstanding (R17), and
// a hole below a set bit costs a CRC anchor, not a re-fetch (#311, #353).
type ResumeResult struct {
	// Durable has one bit per article, set only where this resume proved the
	// article's bytes are on disk.
	Durable Bitmap
	// VerifiedTo is the length of the gapless, verified prefix from byte 0.
	VerifiedTo int64
	// PrefixCRC is the CRC32 of exactly [0, VerifiedTo), whether or not
	// HasPrefixCRC is set.
	PrefixCRC uint32
	// HasPrefixCRC means PrefixCRC is a verified whole-file CRC: the run
	// consumed every recorded fact AND reached the file's end. Anything less
	// is unavailable, which is an honest answer and must stay distinguishable
	// from a CRC of zero (R23).
	HasPrefixCRC bool
	// Recomputed reports that the committed cache was not adopted and the
	// answer came from the file's bytes. It is observability, not a claim:
	// both paths return an equally usable result.
	Recomputed bool
	// Restart reports that the file is gone, so nothing can be resumed
	// against it and every article is Outstanding.
	Restart bool
}

// Resumer answers, from stable storage alone, which of a file's articles still
// need fetching.
type Resumer struct {
	facts FactLog
	exts  ExtentStore
	log   *slog.Logger
}

// NewResumer wires a resumer. It owns none of its collaborators' lifecycles
// and holds no lock: Resume does I/O throughout, is per-file, and shares no
// state between calls, which is how it stays off other jobs' way (R15).
func NewResumer(fl FactLog, es ExtentStore, log *slog.Logger) *Resumer {
	return &Resumer{facts: fl, exts: es, log: log}
}

// Resume establishes what is actually on disk for one file.
//
// The fast path is a stat: if the committed extent's size and mtime still
// match the file, the cache is adopted without reading a byte. Correctness
// never depends on the fast path being right — a mismatch falls through to
// recomputation, and recomputation is correct by definition (S4).
//
// Recomputation reads each recorded region and checks it against the CRC the
// fact log recorded when the article was decoded. That single read produces
// both the verified done-set and the gapless-prefix CRC: verification and CRC
// recovery are the same operation, which is why a resumed file can report a
// real whole-file CRC (R24) rather than the honest absence of one.
//
// An article whose fact carries no CRC (UU-encoded) cannot be verified and is
// therefore left Outstanding. Re-fetching it is cheap; assuming it is correct
// is exactly the optimism the design forbids (S3).
// firstArtIdx is the file's first global article index — lo from the
// manifest's FileRange(fileIdx). FileExtent.Durable is indexed by file-local
// ordinal, so a fact's bit is fact.ArtIdx - firstArtIdx. The barrier gets that
// mapping from SyncTarget.FileLocalOrdinal; this signature has no equivalent,
// so the caller supplies the offset. An index outside [0, artCount) is
// ErrArticleOutOfRange, never a silently clear bit: a clear bit is
// indistinguishable from "those bytes are not on disk" and would re-fetch an
// intact file without saying so (A2, R28).
func (r *Resumer) Resume(ctx context.Context, jobID string, fileIdx int32, path string, firstArtIdx int32, artCount int) (ResumeResult, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing to validate against. Every article is Outstanding,
			// which is S3's safe default rather than a failure.
			r.log.Info("durability resume found no partial file",
				"job", jobID, "file", fileIdx, "path", path)
			return ResumeResult{Durable: NewBitmap(artCount), Restart: true}, nil
		}
		// Any other stat failure is surfaced. Reading it as absence would
		// restart a file whose bytes are still there (A2).
		return ResumeResult{}, fmt.Errorf("durability: resume stat job=%s file=%d: %w", jobID, fileIdx, err)
	}

	ext, ok, err := r.committedExtent(ctx, jobID, fileIdx, artCount)
	if err != nil {
		return ResumeResult{}, err
	}
	if ok && ext.Size == fi.Size() && ext.ModTimeNs == fi.ModTime().UnixNano() {
		// R13's cheap validity check passed. Both halves are load-bearing:
		// a truncation moves the size, an in-place edit of the same length
		// moves only the mtime.
		//
		// HasPrefixCRC is re-checked against the strict rule rather than
		// adopted, because the flag can outlive the condition it asserts. A
		// resume commits it true for a whole file; an article then lands
		// beyond a hole, so the file grows while VerifiedTo does not move;
		// the barrier only clears the flag when VerifiedTo *changes*, so it
		// survives. The next restart's stamp matches, and a partial extent's
		// CRC would be adopted as the file's — #349's misuse re-entering
		// through the cache instead of the walk. The re-check costs one
		// comparison against a stat this path already made, so B3 is intact.
		return ResumeResult{
			Durable:      ext.Durable,
			VerifiedTo:   ext.VerifiedTo,
			PrefixCRC:    ext.PrefixCRC,
			HasPrefixCRC: ext.HasPrefixCRC && ext.VerifiedTo == fi.Size(),
		}, nil
	}
	return r.recompute(ctx, jobID, fileIdx, path, fi.Size(), firstArtIdx, artCount)
}

// committedExtent loads the Class B cache for one file, reporting whether one
// exists at all.
//
// The bitmap is re-derived at artCount bits rather than adopted as loaded, for
// the reason Barrier.priorExtent documents at length: ExtentStore.Load rebuilds
// each bitmap at its full BYTE width, so Bitmap's tail-word mask never fires
// and a damaged blob's padding bits would survive into Count() — the over-claim
// direction. Only a caller holding artCount can apply the mask, and rebuilding
// through BitmapFromBytes is what applies it; narrowing what Load returned
// would not. A stored bitmap narrower than artCount is zero-padded, which reads
// as "those articles are not durable", the safe direction under S3.
func (r *Resumer) committedExtent(ctx context.Context, jobID string, fileIdx int32, artCount int) (FileExtent, bool, error) {
	stored, err := r.exts.Load(ctx, jobID)
	if err != nil {
		return FileExtent{}, false, fmt.Errorf("durability: resume load extent job=%s file=%d: %w", jobID, fileIdx, err)
	}
	for _, e := range stored {
		if e.FileIdx != fileIdx {
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
			return FileExtent{}, false, fmt.Errorf("durability: resume re-derive bitmap job=%s file=%d: %w", jobID, fileIdx, err)
		}
		e.Durable = bm
		return e, true, nil
	}
	return FileExtent{}, false, nil
}

// recompute derives the done-set from the file's bytes, which S4 makes correct
// by definition, and the gapless-prefix CRC from the same read (R24).
func (r *Resumer) recompute(ctx context.Context, jobID string, fileIdx int32, path string, size int64, firstArtIdx int32, artCount int) (ResumeResult, error) {
	facts, err := r.facts.ForFile(ctx, jobID, fileIdx)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("durability: resume facts job=%s file=%d: %w", jobID, fileIdx, err)
	}
	//nolint:gosec // G304: path is the job's own target file, the same path
	// the assembler wrote and the stat above already resolved.
	f, err := os.Open(path)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("durability: resume open job=%s file=%d: %w", jobID, fileIdx, err)
	}
	defer func() { _ = f.Close() }()

	durable := NewBitmap(artCount)
	verified, err := r.verifyRegions(ctx, f, facts, durable, size, firstArtIdx, artCount, jobID, fileIdx)
	if err != nil {
		return ResumeResult{}, err
	}

	prefix, crc, whole := gaplessPrefixCRC(facts, verified, size)
	r.log.Info("durability resume recomputed a file from its bytes",
		"job", jobID, "file", fileIdx, "articles_durable", durable.Count(),
		"facts", len(facts), "verified_to", prefix)
	return ResumeResult{
		Durable:      durable,
		VerifiedTo:   prefix,
		PrefixCRC:    crc,
		HasPrefixCRC: whole,
		Recomputed:   true,
	}, nil
}

// verifyRegions checks each recorded region against its CRC, setting the
// article's bit only on a match, and returns a parallel slice saying which
// facts were proven. The slice is what the prefix walk reads, so verification
// happens exactly once per article for both answers.
//
// A fact with no CRC is skipped rather than trusted: nothing can prove those
// bytes are the right bytes, and S3 makes the unprovable Outstanding.
func (r *Resumer) verifyRegions(ctx context.Context, f *os.File, facts []ArticleFact, durable Bitmap, size int64, firstArtIdx int32, artCount int, jobID string, fileIdx int32) ([]bool, error) {
	verified := make([]bool, len(facts))
	var buf []byte
	for i, fact := range facts {
		// R15: a recomputation over a large file must stop when its caller
		// does rather than run to completion after a shutdown.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("durability: resume recompute job=%s file=%d: %w", jobID, fileIdx, err)
		}
		ord := int(fact.ArtIdx - firstArtIdx)
		if ord < 0 || ord >= artCount {
			return nil, fmt.Errorf("%w: job=%s file=%d article=%d first=%d count=%d",
				ErrArticleOutOfRange, jobID, fileIdx, fact.ArtIdx, firstArtIdx, artCount)
		}
		if !fact.HasCRC {
			continue
		}
		// Offset is bounded against size before the addition, so the sum
		// cannot wrap: a corrupt row with Offset near MaxInt64 would
		// otherwise produce a negative sum that passes a "> size" test, and
		// the guard would read as bounding a region it had not bounded.
		// ReadAt would still fail, but on the wrong claim.
		if fact.Offset < 0 || fact.Length < 0 || fact.Offset > size-int64(fact.Length) {
			// The region the fact describes is not wholly inside the file as
			// it exists now, so its bytes cannot be checked. Outstanding.
			continue
		}
		if int64(cap(buf)) < int64(fact.Length) {
			buf = make([]byte, fact.Length)
		}
		region := buf[:fact.Length]
		if _, err := f.ReadAt(region, fact.Offset); err != nil {
			return nil, fmt.Errorf("durability: resume read job=%s file=%d article=%d: %w",
				jobID, fileIdx, fact.ArtIdx, err)
		}
		if crc32.ChecksumIEEE(region) != fact.CRC32 {
			continue
		}
		durable.Set(ord)
		verified[i] = true
	}
	return verified, nil
}

// gaplessPrefixCRC walks the offset-ordered facts from byte 0 and returns the
// length of the verified, contiguous run, its CRC, and whether that CRC may be
// reported as the file's.
//
// FactLog.ForFile already returns facts ordered by Offset — a Task 4 test pins
// it — so the walk needs no sort of its own. Contiguity from 0 is exactly what
// makes crc32util.Combine valid here: each step appends a range that starts
// where the previous one ended.
//
// The third result is true only when the run consumed every recorded fact AND
// reached the file's current end. Either clause failing means something known
// about this file lies outside the CRC's range — a fact beyond a truncated
// end, or bytes no fact accounts for — so the CRC of that shorter prefix is
// not the file's CRC, and R23 wants unavailable rather than a relabelling.
// PrefixCRC still covers exactly [0, VerifiedTo) in every case.
func gaplessPrefixCRC(facts []ArticleFact, verified []bool, size int64) (verifiedTo int64, prefixCRC uint32, wholeFile bool) {
	var prefix int64
	var crc uint32
	consumed := 0
	for i, fact := range facts {
		if !verified[i] {
			break
		}
		// Not exactly abutting the run so far: either a hole, or an overlap
		// this walk cannot prove tiles the range (R23).
		if fact.Offset != prefix {
			break
		}
		crc = crc32util.Combine(crc, fact.CRC32, int64(fact.Length))
		prefix = fact.Offset + int64(fact.Length)
		consumed++
	}
	return prefix, crc, consumed == len(facts) && prefix == size
}
