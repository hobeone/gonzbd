package job

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/hobeone/gonzbd/internal/durability"
)

// InstallVerified marks each row's article Done and keeps the rows resident
// for the whole-file CRC. It is how a restart's verified rows enter a job;
// the caller has already read each row's bytes back and matched its CRC.
//
// Every row must name fileIdx and an article inside that file's range, or
// nothing is installed. A row for an article that already has one replaces
// it. Like the other done-bit doors it needs the manifest, for the counters.
func (j *Job) InstallVerified(fileIdx int, rows []durability.WrittenRow) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	m := j.manifest
	if fileIdx < 0 || fileIdx >= m.NumFiles() {
		return fmt.Errorf("job %s: fileIdx %d out of range (%d files)", j.id, fileIdx, m.NumFiles())
	}
	lo, hi := m.FileRange(fileIdx)
	for _, r := range rows {
		if r.FileIdx != fileIdx || int(r.ArtIdx) < lo || int(r.ArtIdx) >= hi {
			return fmt.Errorf("job %s: verified row file %d article %d does not belong to file %d [%d,%d)",
				j.id, r.FileIdx, r.ArtIdx, fileIdx, lo, hi)
		}
	}

	p := j.progress
	for _, r := range rows {
		p.markDone(m, int(r.ArtIdx))
	}
	if p.written == nil {
		p.written = make(map[int][]durability.WrittenRow)
	}
	byArt := make(map[int32]durability.WrittenRow, len(p.written[fileIdx])+len(rows))
	for _, r := range p.written[fileIdx] {
		byArt[r.ArtIdx] = r
	}
	for _, r := range rows {
		byArt[r.ArtIdx] = r
	}
	merged := slices.Collect(maps.Values(byArt))
	slices.SortFunc(merged, func(a, b durability.WrittenRow) int {
		return cmp.Or(cmp.Compare(a.Offset, b.Offset), cmp.Compare(a.ArtIdx, b.ArtIdx))
	})
	p.written[fileIdx] = merged
	return nil
}

// FileRows returns a copy of one file's resident written rows, in offset
// order, or nil when the job has none for it.
func (j *Job) FileRows(fileIdx int) []durability.WrittenRow {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return nil
	}
	return slices.Clone(j.progress.written[fileIdx])
}
