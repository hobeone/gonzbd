package job

import (
	"fmt"
	"slices"

	"github.com/hobeone/gonzbd/internal/crc32util"
	"github.com/hobeone/gonzbd/internal/durability"
)

// The resident written rows (JobProgress.written) are what the whole-file CRC
// is derived from. They enter through InstallVerified and
// InstallFileVerification at verification and through MarkArticleWritten as
// articles are written, and leave when the file's CRC is settled
// (SettleFileCRC, or InstallFileVerification itself), when the file is
// untrusted (UntrustFile), or with their article's Done bit
// (JobProgress.clearDone). A resident row's article is Done.
//
// A slice stored in written is never edited in place after it is stored: a
// writer appends, or builds a new slice and stores that. A Progress() clone
// can therefore share the slices rather than copy them.

// InstallVerified marks each row's article Done and keeps the rows resident
// for the whole-file CRC; the caller has already read each row's bytes back
// and matched its CRC. It is the rows step of InstallFileVerification alone,
// which is how a restart's or a retry's rows enter a job; its own callers are
// tests seeding Done articles (`git grep -n '\.[I]nstallVerified(' -- '*.go'
// ':!*_test.go'` returns nothing).
//
// A row that does not name fileIdx, names an article outside that file's
// range, or has an invalid shape (WrittenRow.HasValidShape) is dropped and
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

// placeRows checks fileIdx is in range of m, and splits rows into those that
// belong to the file and a count of those that do not.
func placeRows(m *Manifest, fileIdx int, rows []durability.WrittenRow) (kept []durability.WrittenRow, dropped int, err error) {
	if fileIdx < 0 || fileIdx >= m.NumFiles() {
		return nil, 0, fmt.Errorf("fileIdx %d out of range (%d files)", fileIdx, m.NumFiles())
	}
	kept = make([]durability.WrittenRow, 0, len(rows))
	for _, r := range rows {
		// Rows come from disk. Their range is judged by
		// WrittenRow.HasValidShape, which accepts the zero-length row
		// MarkArticleWritten accepts live.
		if r.FileIdx != fileIdx || !m.ArticleInFile(r.FileIdx, r.ArtIdx) || !r.HasValidShape() {
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
	p.upsertRows(fileIdx, rows)
}

// upsertRows makes each row its article's resident row in p.written[fileIdx],
// replacing the row the article already has or appending one. It is the one
// replace-or-append of resident rows: installRows and Job.MarkArticleWritten
// call it (`git grep -n 'p\.[u]psertRows(' -- '*.go' ':!*_test.go'` returns 2
// lines). MarkArticleWritten calls it only for an article whose Done bit was
// already set; a newly-Done article has no row, and appendRow adds it.
//
// The resident rows are kept in no particular order. Their readers do not
// assume one: FileRows, SettleFileCRC and settleFileCRC each sort a copy
// (sortedClone).
//
// clone() shares p.written's slices, so a stored slice is never edited in
// place: a replacement, or dropRows' removal, is made in a new slice. An
// append, here or in appendRow, may write into the stored slice's spare
// capacity, which no holder can see: every slice header sharing that backing
// array was read from p.written, the stored length only grows between
// replacements, so each holder's length is at most the stored one and append
// writes only past it. A replacement or a removal moves p.written to a new
// array, and nothing appends to the old one again. When the
// file has no resident rows the append starts a new array, so the caller's
// slice is never kept. The caller holds contentMu.
func (p *JobProgress) upsertRows(fileIdx int, rows []durability.WrittenRow) {
	if len(rows) == 0 {
		return
	}
	if p.written == nil {
		p.written = make(map[int][]durability.WrittenRow)
	}
	out := p.written[fileIdx]
	at := make(map[int32]int, len(out)+len(rows))
	for k, r := range out {
		at[r.ArtIdx] = k
	}
	copied := false
	for _, r := range rows {
		k, ok := at[r.ArtIdx]
		if !ok {
			at[r.ArtIdx] = len(out)
			out = append(out, r)
			continue
		}
		if !copied {
			out = slices.Clone(out)
			copied = true
		}
		out[k] = r
	}
	p.written[fileIdx] = out
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
	// The same two predicates placeRows judges a restart's rows by. A
	// zero-length row is valid here: the assembler reports a zero-length
	// article through OnArticleWritten with n == 0, and unless it is Done its
	// file never completes.
	if !m.ArticleInFile(row.FileIdx, row.ArtIdx) {
		return fmt.Errorf("job %s: article %d is not in file %d", j.id, row.ArtIdx, row.FileIdx)
	}
	if !row.HasValidShape() {
		return fmt.Errorf("job %s: article %d has an invalid range [%d, +%d)", j.id, row.ArtIdx, row.Offset, row.Length)
	}
	p := j.progress
	if p.markDone(m, int(row.ArtIdx)) {
		// The bit was clear, so the article has no resident row: clearDone,
		// which clears the bit, drops the row with it.
		p.appendRow(row)
		return nil
	}
	p.upsertRows(row.FileIdx, []durability.WrittenRow{row})
	return nil
}

// appendRow adds row to its file's resident rows, for an article that has
// none. The caller holds contentMu. Like upsertRows' append, it may write
// into the stored slice's spare capacity, which no clone can see.
func (p *JobProgress) appendRow(row durability.WrittenRow) {
	if p.written == nil {
		p.written = make(map[int][]durability.WrittenRow)
	}
	p.written[row.FileIdx] = append(p.written[row.FileIdx], row)
}

// dropRows removes the resident rows of articles arts from file fi, in one
// pass over the file's rows. When any is removed the remaining rows are stored
// in a new slice, so a clone sharing the old one is unaffected. The caller
// holds contentMu.
func (p *JobProgress) dropRows(fi int, arts []int) {
	rows := p.written[fi]
	if len(rows) == 0 || len(arts) == 0 {
		return
	}
	drop := make(map[int32]struct{}, len(arts))
	for _, i := range arts {
		drop[int32(i)] = struct{}{} //nolint:gosec // G115: article index fits in int32
	}
	dropped := func(r durability.WrittenRow) bool { _, ok := drop[r.ArtIdx]; return ok }
	if !slices.ContainsFunc(rows, dropped) {
		return
	}
	p.written[fi] = slices.DeleteFunc(slices.Clone(rows), dropped)
}

// SettleFileCRC derives a completed file's whole-file CRC from its resident
// rows, stores it on the file, and releases the rows. It returns the CRC and
// whether one could be derived; an underivable file stores zero, which par2
// reads as NoCRC.
//
// The sort and the combine, one crc32util.Combine per article, run with
// contentMu released, so this job's MarkArticleWritten and the job's other
// writers do not wait on them. The lock is taken twice: once to read the
// inputs (settleInputs), once to store the result (installSettled). The
// second refuses a result whose rows changed in between and settles again
// under the lock, so the outcome is one that running the settle and the
// change one after the other, in some order, would have produced.
func (j *Job) SettleFileCRC(fileIdx int) (crc uint32, ok bool, err error) {
	in, err := j.settleInputs(fileIdx)
	if err != nil {
		return 0, false, err
	}
	// --- No lock held below this line ---
	if j.settleCombineHook != nil {
		j.settleCombineHook()
	}
	crc, ok = fileCRCFromRows(sortedClone(in.rows), in.failed, in.lo, in.hi)
	return j.installSettled(fileIdx, in, crc, ok)
}

// settleSnapshot is what SettleFileCRC reads under the lock and combines
// outside it. rows is the stored slice itself, not a copy: a stored slice is
// never edited in place (see the top of this file), so it cannot change while
// it is combined.
type settleSnapshot struct {
	rows   []durability.WrittenRow
	failed bool
	lo, hi int
}

// settleInputs takes, under contentMu, the inputs fileCRCFromRows reads for
// one file.
func (j *Job) settleInputs(fileIdx int) (settleSnapshot, error) {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.manifest == nil || j.progress == nil {
		return settleSnapshot{}, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return settleSnapshot{}, fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	lo, hi := j.manifest.FileRange(fileIdx)
	return settleSnapshot{
		rows:   j.progress.written[fileIdx],
		failed: j.progress.anyFailed(lo, hi),
		lo:     lo,
		hi:     hi,
	}, nil
}

// installSettled stores a CRC combined from in, if in is still current;
// otherwise it settles the file again from what is current, under the lock.
//
// in is current when the file's stored row slice is still in.rows itself
// (sameSlice), not merely equal to it. Every store to p.written either
// replaces the slice with a new array, grows its length, or deletes it
// (`git grep -n 'p\.written\[[^]]*\] = \|delete(p\.written' --
// 'internal/job/*.go' ':!*_test.go'` returns 5 lines: upsertRows, appendRow,
// dropRows, storeSettled and UntrustFile). dropRows is reached through
// clearDone, from ResetForRetry, markNotDone (UntrustFile) and resetForReload
// (ClearEmittedForReload) — `git grep -n '\.clearDon[e](' --
// 'internal/job/*.go' ':!*_test.go'` returns 3 lines. So any row written,
// replaced or dropped since makes the result stale, including a row dropped
// and written back with the same value. The range is not compared:
// newManifest is the one writer of a manifest's file ranges
// (`git grep -n 'fileArticleOffsets\[[^]]*\] =' -- 'internal/job/*.go'
// ':!*_test.go'` returns 2 lines, both in it), and RestoreContent re-attaches
// the job's own manifest, so the range is the one in.lo and in.hi hold.
//
// The failed bits are not compared, because while the slice is unchanged no
// change to them can change the result. A bit is set only on an article that
// is not Done (setFailedBits), and is cleared only by clearDone, which drops
// that article's row and so replaces the slice. A combine that yielded a CRC
// had a row for every article of the range; with the slice unchanged none of
// those rows was dropped, so each article stayed Done and no bit could be set.
// When in.failed was true, a bit cleared without replacing the slice belonged
// to an article with no row, which leaves the chain short of an article, so
// the result is NoCRC either way.
func (j *Job) installSettled(fileIdx int, in settleSnapshot, combined uint32, combinedOK bool) (crc uint32, ok bool, err error) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return 0, false, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if !sameSlice(j.progress.written[fileIdx], in.rows) {
		if j.settleRecombineHook != nil {
			j.settleRecombineHook()
		}
		crc, ok = settleFileCRC(j.manifest, j.progress, fileIdx)
		return crc, ok, nil
	}
	j.progress.storeSettled(fileIdx, combined, combinedOK)
	return combined, combinedOK, nil
}

// sameSlice reports whether a and b are the same stored slice: the same
// length over the same backing array. Because a stored slice is never edited
// in place, the same slice has the same contents. An append into spare
// capacity keeps the array and changes only the length, so both are compared.
// Holding b keeps its array alive, so a later allocation cannot take its
// address.
func sameSlice(a, b []durability.WrittenRow) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || &a[0] == &b[0]
}

// settleFileCRC derives and stores a file's CRC in one critical section. The
// caller holds the job's contentMu and has range-checked fileIdx.
// InstallFileVerification calls it, and SettleFileCRC does when the inputs it
// combined went stale.
func settleFileCRC(m *Manifest, p *JobProgress, fileIdx int) (uint32, bool) {
	lo, hi := m.FileRange(fileIdx)
	crc, ok := fileCRCFromRows(sortedClone(p.written[fileIdx]), p.anyFailed(lo, hi), lo, hi)
	p.storeSettled(fileIdx, crc, ok)
	return crc, ok
}

// storeSettled stores a file's derived CRC, zero when ok is false, and
// releases its resident rows. It is the one writer of a derived
// AssembledCRC32: the other two assignments, in UntrustFile and
// ResetForRetry, write zero (`git grep -n 'AssembledCRC3[2] =' -- '*.go'
// ':!*_test.go'` returns 3 lines). The caller holds contentMu.
func (p *JobProgress) storeSettled(fileIdx int, crc uint32, ok bool) {
	if !ok {
		crc = 0
	}
	p.files[fileIdx].AssembledCRC32 = crc
	delete(p.written, fileIdx)
}

// anyFailed reports whether any article of [lo, hi) is failed. The caller
// holds contentMu.
func (p *JobProgress) anyFailed(lo, hi int) bool {
	for i := lo; i < hi; i++ {
		if p.failed.Get(i) {
			return true
		}
	}
	return false
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
	// The rows go in bulk first, a failed article's with them, so each
	// markNotDone below finds no row to drop.
	delete(p.written, fileIdx)
	lo, hi := m.FileRange(fileIdx)
	for i := lo; i < hi; i++ {
		p.markNotDone(fileIdx, i)
	}
	fp := &p.files[fileIdx]
	fp.Complete = false
	fp.AssembledCRC32 = 0
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

// FileState returns one file's state as the record stores it, read under the
// job's read lock without cloning the progress record. ok is false when the
// job has no progress or fi names no file.
func (j *Job) FileState(fi int) (st durability.FileState, ok bool) {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	p := j.progress
	if p == nil || fi < 0 || fi >= len(p.files) {
		return durability.FileState{}, false
	}
	f := p.files[fi]
	return durability.FileState{FileIdx: fi, Complete: f.Complete, Filename: f.Filename, FetchPolicy: uint8(f.Fetch)}, true
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
