package durability

import (
	"fmt"
	"path/filepath"
)

// PostAnomaly reports something structurally wrong with what the servers
// returned, found while walking a file's Class A facts.
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

// overlapFrom turns a walk's classification into a finding, or reports none.
//
// It takes the pair from the walk rather than indexing a facts slice, so there
// is no way for the reporter to be handed a different slice than the one the
// walk ran over. See prefixWalk.overlap for why the pair is derived rather
// than searched for.
//
// The reported range is the INTERSECTION — from where the arrival starts to
// whichever of the two ends first. Those are not the same span whenever the
// arrival is contained in the victim: a 10-byte article inside a 200-byte one
// disputes 10 bytes, and reporting to the victim's end would hand the user a
// range fifteen times too wide to compare against a par2 repair log.
func overlapFrom(w prefixWalk, idx int32, path string) (PostAnomaly, bool) {
	victim, arrival, ok := w.overlap()
	if !ok {
		return PostAnomaly{}, false
	}
	end := min(victim.Offset+int64(victim.Length), arrival.Offset+int64(arrival.Length))
	return PostAnomaly{
		FileIdx: idx,
		Reason:  overlapReason(path, victim.ArtIdx, arrival.ArtIdx, arrival.Offset, end),
	}, true
}

// overlapReason renders an overlap between two durable articles for a human.
//
// It says the post is malformed rather than that the download failed, because
// that is what is true: the file is repairable, par2 will run — #410 stopped
// the barrier publishing a whole-file CRC for a file in this state, so
// QuickCheck reports NoCRC rather than a spurious match — and the user's
// question is why a healthy-looking job needed repairing.
//
// Article indices rather than Message-IDs: the barrier holds facts, and a fact
// carries the manifest index. Reaching for the ID would mean a lookup this
// package has no path to.
func overlapReason(path string, first, second int32, at, end int64) string {
	return fmt.Sprintf(
		"Overlapping segments in %s: articles #%d and #%d both describe bytes "+
			"[%d,%d), so one of them overwrote the other on disk. The post is "+
			"malformed; the file will be repaired from its par2 volumes if they "+
			"cover it.",
		filepath.Base(path), first, second, at, end)
}
