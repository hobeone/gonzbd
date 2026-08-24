package durability

import (
	"fmt"
	"path/filepath"
	"strings"
)

// PostAnomaly reports something structurally wrong with what the servers
// returned, found by comparing a file's durable runs against the file itself.
//
// It is NOT a storage fault and must not be routed as one: the disk behaved,
// the job is not stalled, and no article is marked failed. It is also not an
// error — a checkpoint that finds one has still succeeded, and the file is
// repairable. Only the user's picture of their download changes.
//
// No JobID field. Both callers hold the job ID as a local at the call site — it
// is the value they passed in one line earlier — so carrying it back would
// duplicate what the caller already has. FileIdx is different and must be
// carried: Run iterates a job's files internally, so its caller cannot know
// which file a finding belongs to.
//
// Reason is formatted here rather than by the caller, mirroring the assembler's
// postAnomalyReason: the phrasing belongs beside the data it describes, and the
// barrier has the path in scope through SyncTarget.
type PostAnomaly struct {
	FileIdx int32
	Reason  string
}

// overlapFrom compares a file's durable runs against its size and reports an
// overlap, or reports none.
//
// The rule is §3.3's, and only one of its three cases is a positive signal:
//
//	Σ Length >  size  definite overlap — articles wrote over each other
//	Σ Length == size  no evidence of overlap, which is not proof of absence
//	Σ Length <  size  articles are missing or failed: the ordinary incomplete case
//
// It is deliberately not a complete overlap detector, and the middle row says
// why: a sum cancels a hole of N bytes against an overlap of N bytes. The old
// prefix walk compared adjacent extents structurally and saw that case; this
// cannot, and the loss is bounded by two things. Such a file has a gap, so no
// single run covers its whole article range, so §3.5 withholds its whole-file
// CRC — #387's outcome is closed structurally rather than by this arithmetic.
// And the file is incomplete either way, so par2 fetches recovery volumes and
// repairs both defects. What is lost is a warning on a file the user is
// already told is incomplete.
//
// pathFn is a function rather than a string because resolving a file's path is
// not free — it takes the pipeline's RLock, looks the file up, and copies a
// FileInfo — and Go evaluates arguments before the call. Passing the resolved
// path would pay that on every file of every checkpoint, while almost every
// call returns false on the comparison below. The closure is built only when
// that line is reached and is cheaper than the lock it defers.
func overlapFrom(runs []Run, size int64, idx int32, pathFn func() string) (PostAnomaly, bool) {
	var recorded int64
	for _, r := range runs {
		recorded += r.Length
	}
	if recorded <= size {
		return PostAnomaly{}, false
	}
	return PostAnomaly{
		FileIdx: idx,
		Reason:  overlapReason(pathFn(), recorded, size),
	}, true
}

// collisionFindings turns the collisions one Commit dropped into anomalies,
// one per file rather than one per collision.
//
// Per file, because the barrier's admit latch is keyed on (job, file) and
// would discard the rest anyway, and because the user's question is "what is
// wrong with this file", not "how many times did it happen". The count is kept
// in the message so the collapse loses nothing a reader would act on.
//
// pathFn resolves a file's path, and is a function for the reason overlapFrom's
// is: resolving takes the pipeline's RLock. Here the call is already on the
// rare path — collisions are empty on every ordinary commit — so this is for
// consistency of the seam rather than to avoid a cost.
func collisionFindings(cols []Collision, pathFn func(int32) string) []PostAnomaly {
	if len(cols) == 0 {
		return nil
	}
	byFile := make(map[int32][]Collision)
	order := make([]int32, 0, len(cols))
	for _, c := range cols {
		if _, seen := byFile[c.FileIdx]; !seen {
			order = append(order, c.FileIdx)
		}
		byFile[c.FileIdx] = append(byFile[c.FileIdx], c)
	}
	out := make([]PostAnomaly, 0, len(order))
	for _, idx := range order {
		out = append(out, PostAnomaly{
			FileIdx: idx,
			Reason:  collisionReason(pathFn(idx), byFile[idx]),
		})
	}
	return out
}

// collisionReason renders one file's exact-offset collisions for a human.
//
// It says what was observed and what follows from it, and deliberately does
// NOT say the file is corrupt. Two articles claiming one offset has two
// outcomes and this layer cannot distinguish them: resolved within a single
// open-file episode the file completes SHORT, which is benign and reaches par2
// as an ordinary shortfall; unresolved across a close-handles cycle or a
// restart the later write overwrites the earlier and the file completes WRONG.
// Only the second is corruption. Both need par2, which is why the message can
// be certain about the remedy while staying honest about the cause.
//
// The articles are named because, unlike an overlap, they survive as a pair
// here — the drop has not happened from the record's point of view until this
// commit lands, so both indices are still in hand. overlapReason cannot do
// this and says why.
func collisionReason(path string, cols []Collision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Duplicate segments in %s: ", filepath.Base(path))
	for i, c := range cols {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "articles %d and %d both claim offset %d", c.Kept, c.Dropped, c.Offset)
	}
	b.WriteString(". Only one can be recorded, so the other's bytes are " +
		"unaccounted for and this file's contents cannot be trusted. No " +
		"whole-file checksum is published for it, so par2 will verify and " +
		"repair it from its recovery volumes if they cover it. The post is " +
		"malformed.")
	return b.String()
}

// overlapReason renders an overlap for a human.
//
// It says the post is malformed rather than that the download failed, because
// that is what is true: the file is repairable, par2 will run — a file that
// REACHES this report keeps more than one run and so never publishes a
// whole-file CRC (§3.5), so QuickCheck reports NoCRC rather than a spurious
// match — and the user's question is why a healthy-looking job needed
// repairing.
//
// "Reaches this report" rather than "is overlapped", because the two are not
// the same set. overlapFrom raises this on Σ length exceeding the file's size,
// which takes at least two rows tiling past the end — the partial-overlap
// shape. An EXACT-offset duplicate is a different shape: mergeAdjacentRuns
// drops one of the pair, so it leaves no excess to sum and never gets here at
// all. That case is out of this function's reach and belongs to
// collisionReason, which the commit's own returned Collisions drive.
//
// It names no article, and that is a real loss rather than an omission. A run
// merges the articles that abut into one row, so once the record is written
// there is no longer a pair to name; the excess byte count is what survives.
// par2 repairs at block level and does not consume the distinction.
func overlapReason(path string, recorded, size int64) string {
	return fmt.Sprintf(
		"Overlapping segments in %s: its articles account for %d bytes but the "+
			"file is %d, so %d bytes were written over each other. The post is "+
			"malformed; the file will be repaired from its par2 volumes if they "+
			"cover it.",
		filepath.Base(path), recorded, size, recorded-size)
}
