package app

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
)

// verifyResult is what one verification pass established about a job's files.
// It describes; it changes nothing. The caller commits Verdicts through
// recorder.apply, then installs Verified (installVerification). A hydration
// attaches the job's content between the two (verifyAndAttach); a retry's
// rebuilt job is already attached (verifyRetry).
type verifyResult struct {
	Verdicts []durability.FileVerdict
	Verified map[int][]durability.WrittenRow // per file, rows that matched, in no particular order
	Failed   map[int][]int32                 // per file, articles failed by an intersection
}

// errVerifyFault wraps every non-definitive error verifyJobFiles returns, so a
// hydration (appResidency.verifyAndAttach) can name the faulted file, park the
// job and not settle it Failed; a retry (verifyRetry) returns it and aborts.
type errVerifyFault struct {
	File string
	Err  error
}

func (e *errVerifyFault) Error() string { return fmt.Sprintf("verify %s: %v", e.File, e.Err) }

func (e *errVerifyFault) Unwrap() error { return e.Err }

// verifyBufSize is the one read buffer a pass reuses for every row.
const verifyBufSize = 1 << 20

// preadAt, fsyncFile and openRoot are seams for tests to inject device errors.
var (
	preadAt   = func(f *os.File, b []byte, off int64) (int, error) { return f.ReadAt(b, off) }
	fsyncFile = func(f *os.File) error { return f.Sync() }
	openRoot  = os.OpenRoot
)

// jobFile locates one of a job's files: Dir is the job directory and Name the
// file's name inside it. The verifier opens Name through an os.Root on Dir, so
// a Name that climbs out of Dir, or a symlink inside Dir that points out of
// it, is refused by the open itself.
type jobFile struct {
	Dir, Name string
}

// Path is the file's path, for messages and for callers that open by path.
func (f jobFile) Path() string { return filepath.Join(f.Dir, f.Name) }

// verifyJobFiles reads back every recorded article of every complete=0 file
// that has rows, whatever its fetch policy, and decides what each row is
// worth. It touches no job and no SQLite row; see verifyResult.
//
// Its callers are a hydration (appResidency.verifyAndAttach) and a retry
// (Application.verifyRetry): `git grep -n '[v]erifyJobFiles(' -- '*.go' ':!*_test.go'`
// finds 3 lines, those two calls and its declaration.
//
// locate resolves a recorded filename to its job directory and name. Both
// callers pass the writer's own resolver (pipeline.jobFileLocation), so the
// verifier reads the file the writer wrote under whatever sanitize options
// are configured. Both opens go through an os.Root on the job directory
// (readBackFile, finishFileByPath), which keeps them inside it.
//
// Per file: an empty filename deletes every row, since none can be read
// back; ENOENT deletes every row only when the file's directory exists — at a
// hydration a missing directory (an unmounted download root) is a fault naming
// it, while on a retry it is absence too, unmounted root or not
// (readBackFile); a name that resolves outside the job directory, through
// ".." or a symlink, is an open error other than ENOENT, so a fault; an
// fsync error on the fresh descriptor deletes every row (the file is
// untrusted); a row with an invalid shape (WrittenRow.HasValidShape) is
// deleted unread, while a zero-length row is valid and verifies against CRC 0;
// a CRC mismatch or a short read deletes that row. Of rows whose ranges
// intersect, the first matching one in offset order is kept and
// each other article is failed and its row deleted.
// A file whose articles are then all resolved (fileFinishable) is finished by
// path and gets SetComplete. On a retry an intersection failure does not count
// as resolved, because ResetForRetry is about to clear it.
//
// A complete=1 file is not read: every row is Verified as it stands.
//
// Any other error — an open error other than ENOENT, a failed open of the
// directory (at a hydration, or any non-ENOENT error on a retry), a read
// error, an fsync or truncate error while finishing, a
// cancelled ctx — returns the zero result and an *errVerifyFault naming the
// file or directory.
func verifyJobFiles(ctx context.Context, m *job.Manifest, files []durability.FileRow,
	rows []durability.WrittenRow, locate func(filename string) jobFile, retry bool) (verifyResult, error) {
	byFile := rowsByFile(rows)
	res := verifyResult{
		Verified: make(map[int][]durability.WrittenRow),
		Failed:   make(map[int][]int32),
	}
	var buf []byte
	for _, f := range files {
		fi := f.FileIndex
		fr := byFile[fi]
		if len(fr) == 0 {
			continue
		}
		if f.Complete {
			res.Verified[fi] = fr
			continue
		}
		if f.Filename == "" {
			res.Verdicts = append(res.Verdicts, durability.FileVerdict{FileIdx: fi, DeleteAll: true})
			continue
		}
		loc := locate(f.Filename)
		path := loc.Path()
		if buf == nil {
			buf = make([]byte, verifyBufSize)
		}
		out, err := readBackFile(ctx, loc, fr, buf, retry)
		if err != nil {
			return verifyResult{}, asVerifyFault(path, err)
		}
		v := durability.FileVerdict{FileIdx: fi, DeleteAll: out.deleteAll, DeleteArtIdxs: out.deleted}
		if len(out.verified) > 0 {
			res.Verified[fi] = out.verified
		}
		if len(out.failed) > 0 {
			res.Failed[fi] = out.failed
		}

		finish, err := finishIfResolved(ctx, m, f, loc, out, retry)
		if err != nil {
			return verifyResult{}, &errVerifyFault{File: path, Err: err}
		}
		if finish {
			v.SetComplete = true
		}
		if v.DeleteAll || len(v.DeleteArtIdxs) > 0 || v.SetComplete {
			res.Verdicts = append(res.Verdicts, v)
		}
	}
	return res, nil
}

// asVerifyFault names path in err, unless err already names what faulted.
func asVerifyFault(path string, err error) error {
	if _, ok := errors.AsType[*errVerifyFault](err); ok {
		return err
	}
	return &errVerifyFault{File: path, Err: err}
}

// finishIfResolved finishes one read-back file by path when every article of
// it is resolved, and reports whether it did. out must be what readBackFile
// just returned for loc: finishFileByPath relies on that read-back's fsync.
func finishIfResolved(ctx context.Context, m *job.Manifest, f durability.FileRow, loc jobFile,
	out fileReadback, retry bool) (bool, error) {
	if out.deleteAll || len(out.verified) == 0 {
		return false, nil
	}
	resolved := make(map[int]bool, len(out.verified)+len(out.failed))
	for _, r := range out.verified {
		resolved[int(r.ArtIdx)] = true
	}
	if !retry {
		for _, a := range out.failed {
			resolved[int(a)] = true
		}
	}
	if !fileFinishable(m, f.FileIndex, job.FetchPolicy(f.FetchPolicy), f.Complete,
		func(i int) bool { return resolved[i] }) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var maxEnd int64
	for _, r := range out.verified {
		if r.Length > 0 { // a zero-length article claims no range
			maxEnd = max(maxEnd, r.Offset+r.Length)
		}
	}
	if err := finishFileByPath(loc, maxEnd); err != nil {
		return false, err
	}
	return true, nil
}

// rowsByFile groups rows by file, each group in offset order.
func rowsByFile(rows []durability.WrittenRow) map[int][]durability.WrittenRow {
	out := make(map[int][]durability.WrittenRow)
	for _, r := range rows {
		out[r.FileIdx] = append(out[r.FileIdx], r)
	}
	for _, rs := range out {
		slices.SortFunc(rs, durability.CompareWrittenRows)
	}
	return out
}

// fileReadback is what reading one file's rows back established.
type fileReadback struct {
	deleteAll bool
	deleted   []int32                 // rows to delete: mismatched, short, or failed
	verified  []durability.WrittenRow // resolveRows' rows in offset order, then the zero-length rows
	failed    []int32                 // failed by an intersection
}

// readBackFile opens, fsyncs and drops the cache of one file, then reads each
// valid row and resolves intersections. rows are in offset order. A result
// that is not deleteAll follows a successful fsync of the file on the
// descriptor opened here; finishFileByPath relies on that.
func readBackFile(ctx context.Context, loc jobFile, rows []durability.WrittenRow, buf []byte, retry bool) (fileReadback, error) {
	// Absence is definitive only inside a directory that exists; a missing
	// directory says nothing about the file at a hydration, where it may be
	// a share that has not come up. A retry treats a missing directory
	// (ENOENT opening it) as absence: it cannot tell an unmounted download
	// root from a deleted job directory, and either way drops the rows and
	// refetches. Any other error opening the directory is a fault on both
	// paths. This is the one place that decides fault versus gone.
	root, err := openRoot(loc.Dir)
	if err != nil {
		if retry && errors.Is(err, fs.ErrNotExist) {
			return fileReadback{deleteAll: true}, nil
		}
		return fileReadback{}, &errVerifyFault{File: loc.Dir, Err: err}
	}
	defer func() { _ = root.Close() }() // a directory handle; nothing to lose on close
	fh, err := root.Open(loc.Name)
	if errors.Is(err, fs.ErrNotExist) {
		return fileReadback{deleteAll: true}, nil // inside a directory held open, so one that exists
	}
	if err != nil {
		return fileReadback{}, err
	}
	defer func() { _ = fh.Close() }() // read-only descriptor; nothing to lose on close

	// A writeback error no earlier fsync reported is reported to this fresh
	// descriptor: the file's bytes cannot be trusted.
	if fsyncFile(fh) != nil {
		return fileReadback{deleteAll: true}, nil
	}
	if err := dropPageCache(fh); err != nil {
		return fileReadback{}, err
	}

	// A zero-length row verifies zero bytes against CRC 0 and claims no byte
	// range (docs/durability-contract.md §5), so it stays out of the
	// intersection resolution, which orders rows by the ranges they cover.
	valid := make([]durability.WrittenRow, 0, len(rows))
	var empty []durability.WrittenRow
	var invalid []int32
	for _, r := range rows {
		switch {
		case !r.HasValidShape():
			invalid = append(invalid, r.ArtIdx)
		case r.Length == 0:
			empty = append(empty, r)
		default:
			valid = append(valid, r)
		}
	}
	match := make([]bool, len(valid))
	for k, r := range valid {
		if err := ctx.Err(); err != nil {
			return fileReadback{}, err
		}
		ok, err := rowMatches(fh, r, buf)
		if err != nil {
			return fileReadback{}, err
		}
		match[k] = ok
	}
	out := resolveRows(valid, match)
	out.deleted = append(out.deleted, invalid...)
	for _, r := range empty {
		// Zero bytes have CRC 0, so the row is judged without a read.
		if r.CRC32 == 0 {
			out.verified = append(out.verified, r)
		} else {
			out.deleted = append(out.deleted, r.ArtIdx)
		}
	}
	return out, nil
}

// resolveRows keeps, in offset order, each matching row that does not
// intersect the one kept just before it. A row that intersects a kept row is
// failed, whether or not it matched itself; any other non-matching row is
// deleted and its article is Outstanding again.
//
// rows are in offset order and kept rows are disjoint, so the kept rows are
// ordered by end as well, and a row can only reach the nearest kept row on
// either side of it.
//
// Precondition: rows are sorted by (Offset, ArtIdx) and every Length is > 0;
// readBackFile filters the invalid rows out first.
func resolveRows(rows []durability.WrittenRow, match []bool) fileReadback {
	var out fileReadback
	kept := make([]bool, len(rows))
	for k, r := range rows {
		if !match[k] {
			continue
		}
		if n := len(out.verified); n > 0 && intersects(r, out.verified[n-1]) {
			continue
		}
		kept[k] = true
		out.verified = append(out.verified, r)
	}
	before := 0 // kept rows preceding rows[k]
	for k, r := range rows {
		if kept[k] {
			before++
			continue
		}
		hit := before > 0 && intersects(r, out.verified[before-1]) ||
			before < len(out.verified) && intersects(r, out.verified[before])
		if hit {
			out.failed = append(out.failed, r.ArtIdx)
		}
		out.deleted = append(out.deleted, r.ArtIdx)
	}
	return out
}

func intersects(a, b durability.WrittenRow) bool {
	return a.Offset < b.Offset+b.Length && b.Offset < a.Offset+a.Length
}

// rowMatches reads one row's range through buf and compares its CRC. A short
// read at EOF is a definitive mismatch; every other read error is returned.
func rowMatches(fh *os.File, r durability.WrittenRow, buf []byte) (bool, error) {
	var crc uint32
	for done := int64(0); done < r.Length; {
		chunk := buf[:min(int64(len(buf)), r.Length-done)]
		n, err := preadAt(fh, chunk, r.Offset+done)
		crc = crc32.Update(crc, crc32.IEEETable, chunk[:n])
		done += int64(n)
		switch {
		case n == len(chunk):
		case errors.Is(err, io.EOF):
			return false, nil
		case err != nil:
			return false, err
		default:
			return false, io.ErrNoProgress
		}
	}
	return crc == r.CRC32, nil
}

// finishFileByPath truncates a file whose every article was just read back
// from the device to maxEnd if it is larger and maxEnd > 0, fsyncs it if it
// truncated, and closes it. It never grows a file, and a file no article
// bounds is left alone.
//
// It does not fsync before the truncate, as the assembler's finish does:
// finishIfResolved is its one caller — `git grep -n '[f]inishFileByPath(' -- '*.go' ':!*_test.go'`
// finds 2 lines, that call and this declaration — and runs it only on a
// readBackFile result that is not deleteAll, which follows readBackFile's own
// successful fsync of this file, with only reads in between.
func finishFileByPath(loc jobFile, maxEnd int64) (err error) {
	path := loc.Path()
	root, err := openRoot(loc.Dir)
	if err != nil {
		return fmt.Errorf("finish %s: %w", path, err)
	}
	defer func() { _ = root.Close() }() // a directory handle; nothing to lose on close
	fh, err := root.OpenFile(loc.Name, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("finish %s: %w", path, err)
	}
	defer func() {
		if cErr := fh.Close(); cErr != nil && err == nil {
			err = fmt.Errorf("finish %s: close: %w", path, cErr)
		}
	}()
	if err := fsutil.ShrinkAfterSync(fh, maxEnd, fsyncFile); err != nil {
		return fmt.Errorf("finish %s: %w", path, err)
	}
	return nil
}

// jobFileLocation resolves where one of a job's files lives: the job
// directory, and the sanitized name inside it. registerFile names the writer's
// file with it and the restart verifier reads with it, so the two cannot
// disagree about a file's directory or name — a verification that read a
// different path than the writer used would find every file missing. The
// sanitized name is the one fsutil.JoinSafe would join, a single path
// component (TestJobFileLocation_AgreesWithTheWritersJoin).
func (p *pipeline) jobFileLocation(jobName, filename string) jobFile {
	p.mu.RLock()
	jobDir := filepath.Join(p.downloadDir, jobName)
	sanitize := p.sanitize
	p.mu.RUnlock()
	// --- No lock held below this line ---
	return jobFile{Dir: jobDir, Name: fsutil.SanitizeFilename(filename, sanitize)}
}

// jobFilePath is jobFileLocation's path.
func (p *pipeline) jobFilePath(jobName, filename string) string {
	return p.jobFileLocation(jobName, filename).Path()
}

// uniqueJobFileName returns loc.Name, or the first of its ".1", ".2"… variants that
// nothing in loc.Dir holds yet (fsutil.GetUniqueRelPath, which Lstats through
// an os.Root on loc.Dir, so a symlink counts as taken and is never followed).
// A directory that does not exist or cannot be opened holds no name: loc.Name
// is returned unchanged, and the writer's own open reports any real error.
func uniqueJobFileName(loc jobFile) string {
	root, err := openRoot(loc.Dir)
	if err != nil {
		return loc.Name
	}
	defer func() { _ = root.Close() }() // a directory handle; nothing to lose on close
	return fsutil.GetUniqueRelPath(root, loc.Name)
}

// fileFinishable reports whether one file has every article resolved and no
// Complete flag, and so needs finishing. verifyJobFiles asks it, of the
// articles its read-back resolved.
//
// FetchAlways only, matching Job.IsComplete. policy is the one job_files
// stored (finishIfResolved passes FileRow.FetchPolicy), on a retry as much as
// at a hydration. On a retry that is the failed attempt's policy, while
// installVerification keeps the rebuilt job's: a recovery volume a damage
// verdict released, and that was fetched whole, is finished, and the rebuilt
// job then holds it as FetchIfNeeded with Complete set
// (TestRetryHistoryJob_ResumesCompletedFilesFromTheRecord).
//
// A file with NO articles is excluded: the loop below is vacuously true over
// an empty range.
func fileFinishable(m *job.Manifest, fi int, policy job.FetchPolicy, complete bool, resolved func(i int) bool) bool {
	if m == nil || fi < 0 || fi >= m.NumFiles() {
		return false
	}
	if policy != job.FetchAlways || complete {
		return false
	}
	lo, hi := m.FileRange(fi)
	if hi <= lo {
		return false
	}
	for i := lo; i < hi; i++ {
		if !resolved(i) {
			return false
		}
	}
	return true
}
