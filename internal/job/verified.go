package job

import (
	"fmt"
	"maps"
	"slices"

	"github.com/hobeone/gonzbd/internal/crc32util"
	"github.com/hobeone/gonzbd/internal/durability"
)

// The resident written rows (JobProgress.written) are what the whole-file CRC
// is derived from. They enter through InstallVerified and InstallCompleteFile
// at verification and through MarkArticleWritten as articles are written, and
// leave when the file's CRC is settled (SettleFileCRC) or the file is
// untrusted (UntrustFile).
//
// A slice stored in written is never edited in place after it is stored: a
// writer appends, or builds a new slice and stores that. A Progress() clone
// can therefore share the slices rather than copy them.

// InstallVerified marks each row's article Done and keeps the rows resident
// for the whole-file CRC. It is how a restart's verified rows enter a job;
// the caller has already read each row's bytes back and matched its CRC.
//
// A row that does not name fileIdx, names an article outside that file's
// range, or has a negative offset or a non-positive length is dropped and
// counted in dropped: it costs its own article and nothing else (Standing
// Design Rule 3). A row for an article that already has one replaces it. Like
// the other done-bit doors it needs the manifest, for the counters.
func (j *Job) InstallVerified(fileIdx int, rows []durability.WrittenRow) (dropped int, err error) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	kept, dropped, err := placeRows(j.manifest, fileIdx, rows)
	if err != nil {
		return 0, fmt.Errorf("job %s: %w", j.id, err)
	}
	installRows(j.manifest, j.progress, fileIdx, kept)
	return dropped, nil
}

// InstallCompleteFile installs a complete=1 file at a restart: every row it
// can place is Done, every other article of the file's range is failed, the
// file is Complete, and its whole-file CRC is settled from the rows, which are
// then released. No byte of the file is read: complete=1 was written only
// after the file's fsync (docs/durability-contract.md).
//
// Rows are placed as InstallVerified places them, so a corrupt row costs only
// its own article, which is then failed like any article with no row.
func (j *Job) InstallCompleteFile(fileIdx int, rows []durability.WrittenRow) (dropped int, err error) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	m, p := j.manifest, j.progress
	kept, dropped, err := placeRows(m, fileIdx, rows)
	if err != nil {
		return 0, fmt.Errorf("job %s: %w", j.id, err)
	}
	installRows(m, p, fileIdx, kept)
	lo, hi := m.FileRange(fileIdx)
	for i := lo; i < hi; i++ {
		_ = p.markFailed(m, i) // a no-op for an article a row just marked Done
	}
	p.files[fileIdx].Complete = true
	settleFileCRC(m, p, fileIdx)
	return dropped, nil
}

// placeRows checks fileIdx is in range of m, and splits rows into those that
// belong to the file and a count of those that do not.
func placeRows(m *Manifest, fileIdx int, rows []durability.WrittenRow) (kept []durability.WrittenRow, dropped int, err error) {
	if fileIdx < 0 || fileIdx >= m.NumFiles() {
		return nil, 0, fmt.Errorf("fileIdx %d out of range (%d files)", fileIdx, m.NumFiles())
	}
	kept = make([]durability.WrittenRow, 0, len(rows))
	for _, r := range rows {
		// The shape check is placeRows' own: its rows come from disk, and a
		// row with no bytes vouches for nothing.
		if r.FileIdx != fileIdx || !m.ArticleInFile(r.FileIdx, r.ArtIdx) || r.Offset < 0 || r.Length <= 0 {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	return kept, dropped, nil
}

// installRows marks rows Done and merges them into the file's resident rows,
// replacing an article's earlier row. It stores a copy, never rows itself, so
// the caller keeps its slice. The caller holds the job's contentMu and has
// placed the rows.
func installRows(m *Manifest, p *JobProgress, fileIdx int, rows []durability.WrittenRow) {
	for _, r := range rows {
		p.markDone(m, int(r.ArtIdx))
	}
	if p.written == nil {
		p.written = make(map[int][]durability.WrittenRow)
	}
	resident := p.written[fileIdx]
	if len(resident) == 0 {
		if len(rows) > 0 {
			p.written[fileIdx] = sortedClone(rows)
		}
		return
	}
	byArt := make(map[int32]durability.WrittenRow, len(resident)+len(rows))
	for _, r := range resident {
		byArt[r.ArtIdx] = r
	}
	for _, r := range rows {
		byArt[r.ArtIdx] = r
	}
	p.written[fileIdx] = sortedClone(slices.Collect(maps.Values(byArt)))
}

// MarkArticleWritten records an article whose bytes were written: it is Done,
// and its row joins the file's resident rows for the whole-file CRC. It is the
// recorder's door; the row is already buffered for SQLite when it is called.
//
// A row for an article that already has one replaces it, in a new slice, so a
// clone sharing the old one is unaffected.
func (j *Job) MarkArticleWritten(row durability.WrittenRow) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil || j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	m := j.manifest
	// The range check only: a zero-length row is valid here. The assembler
	// reports a zero-length article through OnArticleWritten with n == 0, and
	// unless it is Done its file never completes.
	if !m.ArticleInFile(row.FileIdx, row.ArtIdx) {
		return fmt.Errorf("job %s: article %d is not in file %d", j.id, row.ArtIdx, row.FileIdx)
	}
	p := j.progress
	p.markDone(m, int(row.ArtIdx))
	if p.written == nil {
		p.written = make(map[int][]durability.WrittenRow)
	}
	resident := p.written[row.FileIdx]
	if k := slices.IndexFunc(resident, func(r durability.WrittenRow) bool { return r.ArtIdx == row.ArtIdx }); k >= 0 {
		replaced := slices.Clone(resident)
		replaced[k] = row
		p.written[row.FileIdx] = replaced
		return nil
	}
	p.written[row.FileIdx] = append(resident, row)
	return nil
}

// SettleFileCRC derives a completed file's whole-file CRC from its resident
// rows, stores it on the file, and releases the rows. It returns the CRC and
// whether one could be derived; an underivable file stores zero, which par2
// reads as NoCRC.
func (j *Job) SettleFileCRC(fileIdx int) (crc uint32, ok bool, err error) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return 0, false, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return 0, false, fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	crc, ok = settleFileCRC(j.manifest, j.progress, fileIdx)
	return crc, ok, nil
}

// settleFileCRC is SettleFileCRC's body. The caller holds the job's contentMu
// and has range-checked fileIdx.
func settleFileCRC(m *Manifest, p *JobProgress, fileIdx int) (uint32, bool) {
	lo, hi := m.FileRange(fileIdx)
	failed := false
	for i := lo; i < hi; i++ {
		if p.failed.Get(i) {
			failed = true
			break
		}
	}
	crc, ok := fileCRCFromRows(sortedClone(p.written[fileIdx]), failed, lo, hi)
	if !ok {
		crc = 0
	}
	p.files[fileIdx].AssembledCRC32 = crc
	delete(p.written, fileIdx)
	return crc, ok
}

// UntrustFile returns every written article of a file to Outstanding: its
// Done bits are cleared (a failed article stays failed), its Complete flag and
// CRC are cleared, and its resident rows are released. It is the in-memory
// half of untrusting a file whose fsync failed; the caller removes the file's
// rows from SQLite.
func (j *Job) UntrustFile(fileIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	m, p := j.manifest, j.progress
	if fileIdx < 0 || fileIdx >= m.NumFiles() {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	lo, hi := m.FileRange(fileIdx)
	for i := lo; i < hi; i++ {
		p.markNotDone(i)
	}
	fp := &p.files[fileIdx]
	fp.Complete = false
	fp.AssembledCRC32 = 0
	delete(p.written, fileIdx)
	p.recompute(m)
	return nil
}

// FileRows returns a copy of one file's resident written rows, in offset
// order, or nil when the job has none for it.
func (j *Job) FileRows(fileIdx int) []durability.WrittenRow {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil || len(j.progress.written[fileIdx]) == 0 {
		return nil
	}
	return sortedClone(j.progress.written[fileIdx])
}

// sortedClone returns rows copied into offset order.
func sortedClone(rows []durability.WrittenRow) []durability.WrittenRow {
	out := slices.Clone(rows)
	slices.SortFunc(out, durability.CompareWrittenRows)
	return out
}

// fileCRCFromRows derives a file's whole-file CRC from its per-article rows,
// given in offset order. It returns false (NoCRC) unless every article of
// [lo, hi) has exactly one row, none failed, the first row is at offset 0,
// and each row starts where the previous one ends — so the chain cannot
// overlap or leave a gap.
func fileCRCFromRows(rows []durability.WrittenRow, failed bool, lo, hi int) (uint32, bool) {
	if failed || hi <= lo || len(rows) != hi-lo {
		return 0, false
	}
	seen := make([]bool, hi-lo)
	var crc uint32
	var end int64
	for k, r := range rows {
		a := int(r.ArtIdx)
		if a < lo || a >= hi || seen[a-lo] || r.Offset != end {
			return 0, false
		}
		seen[a-lo] = true
		if k == 0 {
			crc = r.CRC32
		} else {
			crc = crc32util.Combine(crc, r.CRC32, r.Length)
		}
		end = r.Offset + r.Length
	}
	return crc, true
}
