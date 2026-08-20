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
	// overlapVictim and overlapArrival are the two facts that claim the same
	// bytes, when the walk stopped on a fact that started BELOW the run;
	// hasOverlap says whether it did.
	//
	// The PAIR rather than the index it was found at. An index would only be
	// meaningful alongside the exact slice the walk ran over, and nothing in
	// the type could tie a caller's slice to that one — the reporter would be
	// indexing on trust. The walk holds both facts at the break, so handing
	// them back costs nothing and leaves the reporter no arithmetic to get
	// wrong.
	//
	// An explicit bool rather than a sentinel, because the zero prefixWalk is
	// a real value — buildExtent returns it on four error paths — and a zero
	// ArticleFact would read as a genuine overlap at article 0.
	overlapVictim  ArticleFact
	overlapArrival ArticleFact
	hasOverlap     bool
}

// overlap reports the two facts that claim the same bytes, when the walk
// stopped on a fact that started below the run.
//
// Distinguishing this from the walk's other stop reasons is the whole point.
// The walk halts on three conditions and only this one is a defect in the
// posted data:
//
//   - the predicate rejected the fact — not durable, so nothing is on disk to
//     conflict. Within one process this is also what keeps the assembler's own
//     exact-offset collisions from being reported twice: its loser is resolved
//     permanently failed and never earns a durable bit. Across a restart the
//     assembler's acceptedAt is empty, so two articles at the SAME offset can
//     both be written and both earn durable bits, and the walk does then stop
//     on the second. It is still not a double report, because the assembler is
//     blind in exactly that window — but the reason is the window, not the
//     offsets.
//   - the fact starts ABOVE the run — a hole, which is the ordinary state of
//     a file still downloading.
//   - the fact starts BELOW the run — this. Everything under it is durable and
//     tiles, so the bytes it names were already written by another article.
//
// The overlapped sibling is the fact one position earlier, and that is derived
// rather than searched for: prefix is only ever assigned facts[i].Offset+Length
// at the end of a successful iteration, so at the break it equals the previous
// fact's end exactly. With facts offset-ordered, facts[i-1].Offset <=
// facts[i].Offset < prefix == facts[i-1].End, so the two ranges intersect.
//
// At most ONE overlap per file is found, because the walk stops here. A file
// with two overlaps names the lower one and never the other. This says the file
// is damaged; it is not an inventory of the damage. How often that one finding
// is REPORTED is a separate question, and not this type's to answer — see
// Barrier's report latch.
func (w prefixWalk) overlap() (victim, arrival ArticleFact, ok bool) {
	return w.overlapVictim, w.overlapArrival, w.hasOverlap
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

// overlapAnywhere finds the first pair of durable articles whose byte ranges
// intersect, anywhere in the file.
//
// The thorough counterpart to prefixWalk.overlap, and deliberately a SECOND
// function rather than a replacement for it. The walk classifies for free,
// because it has already stopped on the offending fact — but it can only see
// what lies inside the contiguous verified prefix, since computing that prefix
// REQUIRES halting at the first hole. An overlap above a hole is invisible to
// it. This scan halts at nothing: it skips what the walk would stop on and
// keeps going.
//
// The two must not disagree where both can see, and they cannot: both call an
// overlap "a durable, non-empty fact starting below a durable article's end".
// The walk simply runs out of file first. Both feed the same latch, so the
// finding is reported once whichever of them reaches it.
//
// Only FinalizeFile calls this, and that is a cost decision rather than a
// correctness one. It asks the predicate about EVERY fact — 20,000 ordinal
// lookups on a large file — where the walk stops early and asks about almost
// none. Once per file at finalize is affordable; once per file per checkpoint
// is not. The case that makes it worth paying at all is a file that finalizes
// with a permanent gap: a still-downloading file's hole fills, and a later
// checkpoint's walk then reaches the overlap on its own.
//
// Tracking the last non-overlapping fact is sufficient, and a draft that
// tracked the highest END instead was carrying dead logic. The advance happens
// only when fact.Offset >= top's end, and Length > 0 there, so the fact
// advanced to always ends strictly later than the one it replaces — the two
// rules cannot differ. A contained article never reaches the advance at all,
// because it starts below top's end and is returned as the overlap.
//
// That was established by mutation rather than by reading: replacing the
// highest-end test with an unconditional advance reddened nothing, which is
// what a redundant branch looks like.
func overlapAnywhere(facts []ArticleFact, verified func(i int) bool) (victim, arrival ArticleFact, ok bool) {
	var top ArticleFact
	var have bool
	for i, fact := range facts {
		// A zero-length fact overwrote nothing; an unverified one is not on
		// disk to be overwritten. Skipped, not stopped on.
		if fact.Length == 0 || !verified(i) {
			continue
		}
		if have && fact.Offset < top.Offset+int64(top.Length) {
			return top, fact, true
		}
		top, have = fact, true
	}
	return ArticleFact{}, ArticleFact{}, false
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
	var overlapVictim, overlapArrival, prev ArticleFact
	var hasOverlap bool
	for i, fact := range facts {
		if !verified(i) {
			break
		}
		// A zero-length fact describes no bytes, so it can neither extend the
		// run nor conflict with it: consume it and keep walking.
		//
		// Consumed WITHOUT touching prefix or crc, which the ordinary path
		// below cannot do — it would assign prefix = fact.Offset + 0 and drag
		// the run BACKWARDS to an empty article's offset. Counting it as
		// consumed is nonetheless right: consumedAll asks whether anything
		// known about this file lies outside the CRC's range, and zero bytes
		// never do.
		//
		// Skipping rather than stopping is what keeps a later, real overlap
		// findable. An empty fact below the run does not abut it, so the
		// ordinary path would break the walk there and every fact after it —
		// including the one that genuinely overwrote a sibling — would go
		// unexamined. That is a MISSED warning rather than a false one, which
		// is the worse of the two: the file is silently malformed. prev is
		// deliberately left alone here, so the article any later arrival
		// landed inside is still the last non-empty one.
		//
		// No empty-payload guard exists on the decode path, so this is
		// reachable rather than theoretical: Length comes from len(res.Data).
		if fact.Length == 0 {
			consumed++
			continue
		}
		// Not exactly abutting the run so far: either a hole, or an overlap
		// this walk cannot prove tiles the range (R23).
		//
		// Classified HERE, where the walk knows which it saw, rather than
		// re-derived by a caller from VerifiedTo afterwards. A caller could
		// reach the same answer with one more predicate call, but that would
		// be a second computation of one fact — the shape this file was
		// extracted to delete.
		//
		// consumed > 0 is required, not defensive. It says a previous fact was
		// consumed, so prev holds it — and reaching this branch at consumed ==
		// 0 needs Offset < 0, which the decoder bounds at parse. prev rather
		// than facts[i-1] because the two are equal by construction here and
		// only one of them can be out of range.
		if fact.Offset != prefix {
			if fact.Offset < prefix && consumed > 0 {
				overlapVictim, overlapArrival = prev, fact
				hasOverlap = true
			}
			break
		}
		// Combined from the facts rather than read from the file. Class A
		// persists, so it names every article of the file whichever run
		// fetched it — arithmetic over rows already loaded, with no read of
		// the file, which is why this can sit on the barrier's path (R24).
		crc = crc32util.Combine(crc, fact.CRC32, int64(fact.Length))
		prefix = fact.Offset + int64(fact.Length)
		prev = fact
		consumed++
	}
	return prefixWalk{
		VerifiedTo:     prefix,
		PrefixCRC:      crc,
		consumedAll:    consumed == len(facts),
		overlapVictim:  overlapVictim,
		overlapArrival: overlapArrival,
		hasOverlap:     hasOverlap,
	}
}
