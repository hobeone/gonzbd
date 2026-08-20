package durability

import "github.com/hobeone/gonzbd/internal/crc32util"

// prefixWalk is the result of one verifiedPrefix walk over a file's facts.
//
// It carries consumedAll rather than a finished whole-file boolean because the
// whole-file question is asked twice against the same walk: once in
// buildExtent against pre-allocation's size, and again in FinalizeFile against
// the file's true end after the truncate. Only the size differs between them.
// Re-walking to answer it again costs a full pass of crc32util.Combine — 163 ms
// on a 20,000-article file, measured — for a comparison this already holds the
// inputs for.
//
// wholeFile is a method rather than a second return value so the RULE keeps one
// owner across both askings. Recomputing it at a call site from VerifiedTo and
// Size is exactly the defect this file was extracted to fix; asking the same
// walk twice is not the same thing.
type prefixWalk struct {
	// VerifiedTo is the length of the verified, contiguous run from offset 0.
	VerifiedTo int64
	// PrefixCRC covers exactly [0, VerifiedTo).
	PrefixCRC uint32
	// consumedAll reports whether the walk reached the end of the facts slice.
	// A fact left over means something known about this file lies outside
	// PrefixCRC's range.
	consumedAll bool
}

// wholeFile reports whether this walk may be published as the file's whole-file
// CRC, against the size the file has now.
//
// True only when the run is non-empty, consumed every recorded fact, AND
// reached the file's current end.
//
// The last two clauses are the relabelling guard: either failing means
// something known about this file lies outside the CRC's range — a fact beyond
// a truncated end, or bytes no fact accounts for — so the CRC of that shorter
// prefix is not the file's CRC, and R23 wants unavailable rather than a
// relabelling.
//
// The consumed clause is what an overlapping article trips, and it is the one
// Barrier lacked. Facts are built from each article's own decoded bytes before
// the write (see the pipeline's Class A append), so they describe the file that
// SHOULD have been written. If an article overlaps a sibling without extending
// the file, the walk still tiles to the end from the others and the leftover
// fact is the only signal that the bytes on disk may not match. Without this
// clause the barrier published that CRC, it matched par2's, QuickCheck reported
// clean, and the recovery volumes were never fetched (#387).
//
// The non-empty clause is about a different case and is not redundant with the
// others. A zero-length file with no facts satisfies both: the run consumes
// every fact because there are none, and reaches the end because the end is 0.
// The CRC it would report is 0, which is genuinely the CRC32 of zero bytes — so
// the value is right and the CLAIM is wrong, since nothing was verified. A
// target file is zero-length from the moment the assembler creates it until its
// first write lands (with pre-allocation off), and a resume in that window
// would hand QuickCheck a whole-file CRC to compare against par2's hash for the
// real file.
//
// One caller NARROWS this result rather than re-deriving it: Resumer's cache
// fast path additionally requires the extent's VerifiedTo to match the file's
// current size, because a file whose size moved under a cached extent has
// invalidated it. Narrowing a claim the guard already allowed cannot
// manufacture one it refused, which is why that is safe where the barrier's old
// call-site derivation was not.
func (w prefixWalk) wholeFile(size int64) bool {
	return w.VerifiedTo > 0 && w.consumedAll && w.VerifiedTo == size
}

// verifiedPrefix walks a file's offset-ordered facts from zero, combining the
// CRC of every fact that both abuts the run so far and satisfies verified.
//
// It is the sole owner of that walk. It was extracted from two
// independently-maintained copies — Barrier.gaplessPrefix and Resumer's
// gaplessPrefixCRC — which had already diverged: the resume copy carried the
// consumed clause and the barrier copy did not, because the barrier computed
// its flag at the CALL SITE from VerifiedTo and Size alone. That divergence is
// the defect, not an incidental duplication.
//
// verified is a predicate rather than a slice because that is the only thing
// the two callers disagree about. Barrier decides durability through a bitmap
// reached via SyncTarget.FileLocalOrdinal; Resumer has already materialised a
// []bool parallel to facts, so the INDEX is what both can answer to — a
// predicate taking the ArticleFact itself would leave Resumer unable to look up
// its own slice. It also lets Barrier skip the ordinal lookup for facts past
// the break.
//
// FactLog.ForFile returns facts ordered by Offset, so this needs no sort of its
// own. It takes the facts rather than loading them: callers have already read
// them for the other walks over the same file, and a read failure is theirs to
// surface, since committing VerifiedTo = 0 as if it were derived would be a
// silent wrong answer that A2 and R28 forbid.
func verifiedPrefix(facts []ArticleFact, verified func(i int) bool) prefixWalk {
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
	return prefixWalk{VerifiedTo: prefix, PrefixCRC: crc, consumedAll: consumed == len(facts)}
}
