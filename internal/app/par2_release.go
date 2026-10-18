package app

import (
	"fmt"
	"math"
	"slices"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/par2"
)

// recoveryDeficit sizes the release an outcomeRepair verdict makes. When the
// assessment cannot size it, it returns job.Unsized() and why.
//
// Damage that leaves the written rows whole (a CRC mismatch, or an entry that
// could not be checked) is sized by verify, a block-level par2 verify whose
// BlocksNeeded is already net of the recovery blocks on disk. Otherwise the
// release is the par2 slices the identified files are missing, less the
// recovery blocks already fetched intact. When both are available the larger
// wins.
//
// The slice size and file sizes are read from the post's par2 index, so they
// are bounded before any arithmetic: a file size above math.MaxInt64 is
// refused (job.DamagedSlices works in int64), and the slice total is capped at
// job.MaxPar2Slices, so the damaged count stays within it. Block counts come
// from names, which job.RecoveryVolumeBlocks bounds; job.Job.ReleaseRecovery
// enforces its own bound on the need and the cover's size.
//
// loadRows is called at most once, and verify at most once and only for the
// damage above; neither is called when the rest of the assessment refuses
// sizing.
func recoveryDeficit(
	m *job.Manifest, p *job.JobProgress, a par2.Assessment,
	loadRows func() ([]durability.WrittenRow, error),
	verify func() (par2.VerifyResult, error),
) (sizing job.RecoverySizing, why string) {
	verified := a.CRC.Mismatched > 0 || a.CRC.Unverified > 0
	sliceSize, ok := a.ID.SliceSize()
	if !ok {
		return job.Unsized(), "no single par2 set gives a slice size"
	}
	// The entries the engine counts blocks for: every identified file, and
	// the entries nothing delivered matched.
	sizes := make([]uint64, 0, len(a.ID.Files)+len(a.ID.Unaccounted))
	for _, f := range a.ID.Files {
		sizes = append(sizes, f.Desc.FileSize)
	}
	for _, fd := range a.ID.Unaccounted {
		if fd.Undescribed {
			return job.Unsized(), "a par2 entry has no file description"
		}
		sizes = append(sizes, fd.FileSize)
	}
	var total uint64
	for _, size := range sizes {
		if size > math.MaxInt64 {
			return job.Unsized(), "a par2 file size is out of range"
		}
		total += size / sliceSize
		if size%sliceSize != 0 {
			total++
		}
		if total > job.MaxPar2Slices {
			return job.Unsized(), fmt.Sprintf("the par2 set has more than %d slices", job.MaxPar2Slices)
		}
	}

	var verifiedNeed int
	if verified {
		res, err := verify()
		if err != nil {
			return job.Unsized(), fmt.Sprintf("block-level verify failed: %v", err)
		}
		if uint64(res.DataBlocks) != total { //nolint:gosec // G115: DataBlocks is a block count, never negative
			return job.Unsized(), fmt.Sprintf("block-level verify counted %d data blocks, the index describes %d", res.DataBlocks, total)
		}
		verifiedNeed = res.BlocksNeeded
	}

	pos := a.DeliveredIndices()
	if !verified && slices.Contains(pos, -1) {
		return job.Unsized(), "a par2 entry has no delivered file"
	}
	rows, err := loadRows()
	if err != nil {
		if verified {
			return job.NeedBlocks(verifiedNeed), ""
		}
		return job.Unsized(), fmt.Sprintf("written rows unreadable: %v", err)
	}
	byFile := rowsByFile(rows)
	var damaged int
	for i, f := range a.ID.Files {
		if pos[i] >= 0 {
			damaged += job.DamagedSlices(byFile[pos[i]], f.Desc.FileSize, sliceSize)
		}
	}
	// A volume with a failed article or not yet complete counts nothing.
	return job.NeedBlocks(max(verifiedNeed, damaged-p.FetchedRecoveryBlocks(m))), ""
}
