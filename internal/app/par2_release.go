package app

import (
	"fmt"
	"math"
	"slices"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/par2"
)

// maxPar2Slices is the PAR2 limit on the input slices of one recovery set.
const maxPar2Slices = 32768

// recoveryDeficit sizes the release an outcomeRepair verdict makes: the par2
// slices the identified files are missing, less the recovery blocks already
// fetched intact. When the assessment cannot size it, it returns
// job.Unsized() and why.
//
// The slice size and file sizes are read from the post's par2 index, so they
// are bounded before any arithmetic: a file size above math.MaxInt64 is
// refused (job.DamagedSlices works in int64), and the slice total is capped at
// maxPar2Slices, which bounds the need and so the cover
// job.Job.ReleaseRecovery computes from it.
//
// loadRows is called at most once, and only when the rest of the assessment
// allows sizing.
func recoveryDeficit(m *job.Manifest, p *job.JobProgress, a par2.Assessment, loadRows func() ([]durability.WrittenRow, error)) (sizing job.RecoverySizing, why string) {
	switch {
	case a.CRC.Mismatched > 0:
		return job.Unsized(), "a file does not match its par2 checksum"
	case a.CRC.Unverified > 0:
		return job.Unsized(), "a par2 entry could not be checked"
	}
	sliceSize, ok := a.ID.SliceSize()
	if !ok {
		return job.Unsized(), "no single par2 set gives a slice size"
	}
	var total uint64
	for _, f := range a.ID.Files {
		if f.Desc.FileSize > math.MaxInt64 {
			return job.Unsized(), "a par2 file size is out of range"
		}
		total += f.Desc.FileSize / sliceSize
		if f.Desc.FileSize%sliceSize != 0 {
			total++
		}
		if total > maxPar2Slices {
			return job.Unsized(), fmt.Sprintf("the par2 set has more than %d slices", maxPar2Slices)
		}
	}
	pos := a.DeliveredIndices()
	if slices.Contains(pos, -1) {
		return job.Unsized(), "a par2 entry has no delivered file"
	}
	rows, err := loadRows()
	if err != nil {
		return job.Unsized(), fmt.Sprintf("written rows unreadable: %v", err)
	}
	byFile := rowsByFile(rows)
	var damaged int
	for i, f := range a.ID.Files {
		damaged += job.DamagedSlices(byFile[pos[i]], f.Desc.FileSize, sliceSize)
	}
	// A volume with a failed article is counted as contributing nothing,
	// since how many of its blocks survived is not known here.
	var available int
	for fi := range m.NumFiles() {
		if !m.FileIsPar2Recovery(fi) || p.FileFetchPolicy(fi) != job.FetchAlways || p.FileFailedBytes(fi) > 0 {
			continue
		}
		if n, ok := job.RecoveryVolumeBlocks(m.FileSubject(fi)); ok {
			available += n
		}
	}
	return job.NeedBlocks(damaged - available), ""
}
