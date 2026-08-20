package durability

import "github.com/hobeone/gonzbd/internal/crc32util"

// verifiedPrefix walks a file's offset-ordered facts from zero, combining the
// CRC of every fact that both abuts the run so far and satisfies verified.
//
// It is the sole owner of that walk and of the whole-file question. It was
// extracted from two independently-maintained copies — Barrier.gaplessPrefix
// and Resumer's gaplessPrefixCRC — which had already diverged: the resume copy
// carried the consumed clause below and the barrier copy did not, because the
// barrier computed its flag at the CALL SITE from VerifiedTo and Size alone.
// That divergence is the defect, not an incidental duplication. Do not
// reintroduce a caller-side computation of the third result.
//
// verified is a predicate rather than a slice because that is the only thing
// the two callers disagree about. Barrier decides durability through a bitmap
// reached via SyncTarget.FileLocalOrdinal; Resumer has already materialised a
// []bool. A predicate lets each supply what it holds, and lets Barrier skip the
// ordinal lookup for facts past the break.
//
// The third result is true only when the run is non-empty, consumed every
// recorded fact, AND reached the file's current end. The last two clauses are
// the relabelling guard: either failing means something known about this file
// lies outside the CRC's range — a fact beyond a truncated end, or bytes no
// fact accounts for — so the CRC of that shorter prefix is not the file's CRC,
// and R23 wants unavailable rather than a relabelling.
//
// The consumed clause is what an overlapping article trips. Facts are built
// from each article's own decoded bytes before the write (see the pipeline's
// Class A append), so they describe the file that SHOULD have been written. If
// an article overlaps a sibling without extending the file, the walk still
// tiles to the end from the others and the leftover fact is the only signal
// that the bytes on disk may not match. Without this clause the barrier
// published that CRC, it matched par2's, QuickCheck reported clean, and the
// recovery volumes were never fetched (#387).
//
// The first clause is about a different case, and it is not redundant with
// them. A zero-length file with no facts satisfies both: the run consumes
// every fact because there are none, and reaches the end because the end is 0.
// The CRC it would report is 0, which is genuinely the CRC32 of zero bytes —
// so the value is right and the CLAIM is wrong, since nothing was verified.
// A target file is zero-length from the moment the assembler creates it until
// its first write lands (with pre-allocation off), and a resume in that window
// would hand QuickCheck a whole-file CRC to compare against par2's hash for
// the real file.
//
// prefixCRC still covers exactly [0, verifiedTo) in every case.
//
// FactLog.ForFile returns facts ordered by Offset, so this needs no sort of its
// own. It takes the facts rather than loading them: callers have already read
// them for the other walks over the same file, and a read failure is theirs to
// surface, since committing verifiedTo = 0 as if it were derived would be a
// silent wrong answer that A2 and R28 forbid.
func verifiedPrefix(facts []ArticleFact, verified func(i int) bool, size int64) (verifiedTo int64, prefixCRC uint32, wholeFile bool) {
	var prefix int64
	var crc uint32
	consumed := 0
	for i, fact := range facts {
		if !verified(i) {
			break
		}
		// Not exactly abutting the run so far: either a hole, or an overlap
		// this walk cannot prove tiles the range (R23).
		if fact.Offset != prefix {
			break
		}
		// Combined from the facts rather than read from the file. Class A
		// persists, so it names every article of the file whichever run
		// fetched it — arithmetic over rows already loaded, with no read of
		// the file, which is why this can sit on the barrier's path (R24).
		crc = crc32util.Combine(crc, fact.CRC32, int64(fact.Length))
		prefix = fact.Offset + int64(fact.Length)
		consumed++
	}
	return prefix, crc, prefix > 0 && consumed == len(facts) && prefix == size
}
