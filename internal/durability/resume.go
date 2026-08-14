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
	// BytesDurable is the summed Length of the facts this result proved
	// durable. It is carried rather than re-derived because committing an
	// extent without it would zero the figure the API reports for the file.
	BytesDurable int64
	// Size and ModTimeNs are the file's stat at the moment this result was
	// produced. They are R13's validity stamp, and a written-back extent
	// carrying anything else fails S7 on the next start and throws away a
	// cache that is actually valid.
	Size      int64
	ModTimeNs int64
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
// An article whose bytes do not hash to the CRC recorded for them is left
// Outstanding. Re-fetching it is cheap; assuming it is correct is exactly the
// optimism the design forbids (S3). Every fact carries a CRC, so a failure to
// verify always means a mismatch or a region outside the file — never a
// missing checksum.
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
			BytesDurable: ext.BytesDurable,
			Size:         ext.Size,
			ModTimeNs:    ext.ModTimeNs,
		}, nil
	}
	res, err := r.recompute(ctx, jobID, fileIdx, path, fi.Size(), firstArtIdx, artCount)
	if err != nil {
		return ResumeResult{}, err
	}
	res.ModTimeNs = fi.ModTime().UnixNano()

	// Write the recomputation back over the record it disproved.
	//
	// This makes the Resumer a second writer of Class B, which the original
	// design forbade on the grounds that "a committed extent claims a
	// completed fsync stands behind it, and a resume does not perform that
	// fsync". The premise conflates the mechanism with the property: the fsync
	// exists to make bytes survive a restart, and reading those bytes back
	// AFTER a restart and matching their recorded CRC observes that same
	// property directly, with strictly better evidence.
	//
	// The cost of not doing it was mis-stated as "re-verification on the next
	// restart is bounded rework". There is no re-verification. Nothing clears
	// a bit in the store, priorExtent ORs the stored bitmap as its base, so
	// the next checkpoint re-commits the disproven bit with a fresh stamp that
	// then validates — and the next start's fast path adopts it without
	// reading a byte. The bit does not cost rework; it comes back.
	//
	// Safe as a second writer because of WHEN it runs: the startup sweep
	// completes before the downloader can dispatch, so no barrier is running
	// for any job and there is exactly one writer at this moment.
	//
	// Only after a recomputation. The fast path adopted what is already
	// stored, so committing it would rewrite a row with its own contents.
	if err := r.writeBack(ctx, jobID, fileIdx, res); err != nil {
		return ResumeResult{}, err
	}
	return res, nil
}

// committedExtent loads the Class B cache for one file, reporting whether one
// exists at all.
//
// The bitmap is re-derived at artCount bits rather than adopted as loaded, for
// the reason Barrier.priorExtent documents at length: the store rebuilds each
// bitmap at its full BYTE width, so Bitmap's tail-word mask never fires and a
// damaged blob's padding bits would survive into Count() — the over-claim
// direction. Only a caller holding artCount can apply the mask, and rebuilding
// through BitmapFromBytes is what applies it; narrowing what the store returned
// would not. A stored bitmap narrower than artCount is zero-padded, which reads
// as "those articles are not durable", the safe direction under S3.
func (r *Resumer) committedExtent(ctx context.Context, jobID string, fileIdx int32, artCount int) (FileExtent, bool, error) {
	e, ok, err := r.exts.LoadFile(ctx, jobID, fileIdx)
	if err != nil {
		return FileExtent{}, false, fmt.Errorf("durability: resume load extent job=%s file=%d: %w", jobID, fileIdx, err)
	}
	if !ok {
		return FileExtent{}, false, nil
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

// writeBack commits a recomputed result as the file's Class B record.
//
// Every field is filled from the result rather than merged into the stored
// row, because the stored row is exactly what the recomputation disproved:
// carrying any part of it forward would preserve the claim being corrected.
// BytesDurable in particular has to come from the recomputation — committing
// without it would zero the figure the API reports for this file.
func (r *Resumer) writeBack(ctx context.Context, jobID string, fileIdx int32, res ResumeResult) error {
	ext := FileExtent{
		FileIdx:      fileIdx,
		Durable:      res.Durable,
		VerifiedTo:   res.VerifiedTo,
		PrefixCRC:    res.PrefixCRC,
		HasPrefixCRC: res.HasPrefixCRC,
		BytesDurable: res.BytesDurable,
		Size:         res.Size,
		ModTimeNs:    res.ModTimeNs,
	}
	if err := r.exts.Commit(ctx, jobID, []FileExtent{ext}); err != nil {
		return fmt.Errorf("durability: resume write-back job=%s file=%d: %w", jobID, fileIdx, err)
	}
	return nil
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

	// Summed here rather than by the caller: verified is parallel to facts and
	// is the only place that knows which regions were proven, so deriving it
	// anywhere else would re-walk the same two slices to reach the same answer.
	var bytesDurable int64
	for i, f := range facts {
		if verified[i] {
			bytesDurable += int64(f.Length)
		}
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
		BytesDurable: bytesDurable,
		Size:         size,
	}, nil
}

// verifyRegions checks each recorded region against its CRC, setting the
// article's bit only on a match, and returns a parallel slice saying which
// facts were proven. The slice is what the prefix walk reads, so verification
// happens exactly once per article for both answers.
//
// Every fact carries a CRC, so there is no unverifiable-article case to skip.
// That used to be a branch here, guarded by ArticleFact.HasCRC, and it existed
// for UU-encoded articles. It is gone because the premise was wrong: the CRC
// is a checksum of the bytes this program decoded and wrote, so whether the
// article's own format carried one is irrelevant. downloader.decodePayload now
// computes it on the UU path, which closes the set — no decode path yields
// bytes without a checksum over them.
//
// A fact can still fail to prove anything, and those cases remain below: a
// region outside the file as it exists now, and a CRC that does not match. Both
// leave the article Outstanding, per S3.
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
// The third result is true only when the run is non-empty, consumed every
// recorded fact, AND reached the file's current end. The last two clauses are
// the relabelling guard: either failing means something known about this file
// lies outside the CRC's range — a fact beyond a truncated end, or bytes no
// fact accounts for — so the CRC of that shorter prefix is not the file's CRC,
// and R23 wants unavailable rather than a relabelling.
//
// The first clause is about a different case, and it is not redundant with
// them. A zero-length file with no facts satisfies both: the run consumes
// every fact because there are none, and reaches the end because the end is 0.
// The CRC it would report is 0, which is genuinely the CRC32 of zero bytes —
// so the value is right and the CLAIM is wrong, since nothing was verified.
// A target file is zero-length from the moment the assembler creates it until
// its first write lands (with pre-allocation off), and a resume in that window
// would hand QuickCheck a whole-file CRC to compare against par2's hash for
// the real file. Barrier.buildExtent has always required verified > 0 here.
//
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
	return prefix, crc, prefix > 0 && consumed == len(facts) && prefix == size
}
