package durability

import (
	"fmt"
	"path/filepath"
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
// cannot, and the loss is bounded by two things. Such a file has a gap, so it
// has more than one run, so §3.5 withholds its whole-file CRC on the ROW COUNT
// — #387's outcome is closed structurally rather than by this arithmetic. And
// the file is incomplete either way, so par2 fetches recovery volumes and
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

// overlapReason renders an overlap for a human.
//
// It says the post is malformed rather than that the download failed, because
// that is what is true: the file is repairable, par2 will run — an overlapped
// file keeps more than one run and so never publishes a whole-file CRC (§3.5),
// so QuickCheck reports NoCRC rather than a spurious match — and the user's
// question is why a healthy-looking job needed repairing.
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
