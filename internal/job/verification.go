package job

import (
	"fmt"

	"github.com/hobeone/gonzbd/internal/durability"
)

// FileVerification is one file's verification outcome at a restart or a
// retry, as Job.InstallFileVerification applies it.
type FileVerification struct {
	FileIdx int
	// Filename is the recorded filename, restored when non-empty.
	Filename string
	// RestorePolicy asks for Policy, the recorded fetch policy, to be
	// restored. A hydration asks for it, because the record is the current
	// truth; a retry does not, and keeps the policy its rebuilt job derived.
	RestorePolicy bool
	Policy        FetchPolicy
	// Complete is the record's complete=1. Rows are then every row the file
	// has, every other article of the file's range is failed, and the file is
	// Complete and settled. No byte of the file is read: complete=1 was
	// written only after the file's fsync (docs/durability-contract.md).
	// Otherwise Rows are the rows the read-back verified.
	Complete bool
	Rows     []durability.WrittenRow
	// Failed are the articles, by global index, an intersection lost.
	Failed []int32
	// Settle is set when the verifier finished the file by path: its CRC is
	// settled from the installed rows. The caller marks it complete
	// (MarkFileComplete) once it has done what must precede the mark.
	Settle bool
}

// InstallFileVerification applies one file's verification outcome under a
// single hold of contentMu, so no reader sees part of it. In order: the
// filename and, when asked, the fetch policy are restored; the rows are
// installed (Done, and resident for the CRC); for a complete=1 file every
// other article of its range is failed and the file is Complete; the
// intersection's losers are failed; and the CRC is settled, which derives it
// from the installed rows and releases them, so it comes last.
//
// A row that cannot be placed is dropped and counted in dropped: it costs its
// own article and nothing else (Standing Design Rule 3), as does a Failed index
// out of range. It returns an error, having changed nothing, when the job is
// not resident or FileIdx names no file.
func (j *Job) InstallFileVerification(v FileVerification) (dropped int, err error) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	m, p, fi := j.manifest, j.progress, v.FileIdx
	kept, dropped, err := placeRows(m, fi, v.Rows)
	if err != nil {
		return 0, fmt.Errorf("job %s: %w", j.id, err)
	}
	if v.Filename != "" {
		p.files[fi].Filename = v.Filename
	}
	if v.RestorePolicy {
		p.setFileFetchPolicy(fi, v.Policy)
	}
	installRows(m, p, fi, kept)
	if v.Complete {
		lo, hi := m.FileRange(fi)
		for i := lo; i < hi; i++ {
			_ = p.markFailed(m, i) // a no-op for an article a row just marked Done
		}
		p.files[fi].Complete = true
	}
	for _, a := range v.Failed {
		_ = j.markArticleFailed(int(a))
	}
	if v.Complete || v.Settle {
		settleFileCRC(m, p, fi)
	}
	return dropped, nil
}
