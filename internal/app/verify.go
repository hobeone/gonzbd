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

	"github.com/hobeone/gonzbd/internal/crc32util"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
)

// verifyResult is what one verification pass established about a job's files.
// It describes; it changes nothing. The caller commits Verdicts through
// recorder.apply, then attaches the job's content, then installs Verified.
type verifyResult struct {
	Verdicts []durability.FileVerdict
	Verified map[int][]durability.WrittenRow // per file, rows that matched, in offset order
	Failed   map[int][]int32                 // per file, articles failed by an intersection
}

// errVerifyFault wraps every non-definitive error verifyJobFiles returns, so
// reconcileResidency can park the job instead of settling it Failed.
type errVerifyFault struct {
	File string
	Err  error
}

func (e *errVerifyFault) Error() string { return fmt.Sprintf("verify %s: %v", e.File, e.Err) }

func (e *errVerifyFault) Unwrap() error { return e.Err }

// verifyBufSize is the one read buffer a pass reuses for every row.
const verifyBufSize = 1 << 20

// preadAt and fsyncFile are seams for tests to inject device errors.
var (
	preadAt   = func(f *os.File, b []byte, off int64) (int, error) { return f.ReadAt(b, off) }
	fsyncFile = func(f *os.File) error { return f.Sync() }
)

// verifyJobFiles reads back every recorded article of every complete=0 file
// that has rows, whatever its fetch policy, and decides what each row is
// worth. It touches no job and no SQLite row; see verifyResult.
//
// pathFor resolves a recorded filename to a path. The caller passes the
// writer's own resolver (pipeline.jobFilePath), so the verifier reads the
// file the writer wrote under whatever sanitize options are configured.
//
// Per file: an empty filename deletes every row, since none can be read
// back; ENOENT deletes every row only when the file's directory exists — a
// missing directory (an unmounted download root) is a fault naming it; an
// fsync error on the fresh descriptor deletes every row (the file is
// untrusted); a row with a negative offset or a non-positive length is
// deleted unread; a CRC mismatch or a short read deletes that row. Of rows
// whose ranges intersect, the first matching one in offset order is kept and
// each other article is failed and its row deleted.
// A file whose articles are then all resolved (fileFinishable) is finished by
// path and gets SetComplete. On a retry an intersection failure does not count
// as resolved, because ResetForRetry is about to clear it.
//
// A complete=1 file is not read: every row is Verified as it stands.
//
// Any other error — an open error other than ENOENT, a failed stat of the
// directory, a read error, an fsync or truncate error while finishing, a
// cancelled ctx — returns the zero result and an *errVerifyFault naming the
// file or directory.
func verifyJobFiles(ctx context.Context, m *job.Manifest, files []durability.FileRow,
	rows []durability.WrittenRow, pathFor func(filename string) string, retry bool) (verifyResult, error) {
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
		path := pathFor(f.Filename)
		if buf == nil {
			buf = make([]byte, verifyBufSize)
		}
		out, err := readBackFile(ctx, path, fr, buf)
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

		finish, err := finishIfResolved(ctx, m, f, path, out, retry)
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
// it is resolved, and reports whether it did.
func finishIfResolved(ctx context.Context, m *job.Manifest, f durability.FileRow, path string,
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
		maxEnd = max(maxEnd, r.Offset+r.Length)
	}
	if err := finishFileByPath(path, maxEnd); err != nil {
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
	verified  []durability.WrittenRow // in offset order
	failed    []int32                 // failed by an intersection
}

// readBackFile opens, fsyncs and drops the cache of one file, then reads each
// valid row and resolves intersections. rows are in offset order.
func readBackFile(ctx context.Context, path string, rows []durability.WrittenRow, buf []byte) (fileReadback, error) {
	fh, err := os.Open(path) //nolint:gosec // G304: the caller's resolver confines path to the job directory
	if errors.Is(err, fs.ErrNotExist) {
		// Absence is definitive only inside a directory that exists; a
		// missing directory says nothing about the file.
		dir := filepath.Dir(path)
		if _, sErr := os.Stat(dir); sErr != nil {
			return fileReadback{}, &errVerifyFault{File: dir, Err: sErr}
		}
		return fileReadback{deleteAll: true}, nil
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

	valid := make([]durability.WrittenRow, 0, len(rows))
	var invalid []int32
	for _, r := range rows {
		if r.Offset < 0 || r.Length <= 0 {
			invalid = append(invalid, r.ArtIdx)
			continue
		}
		valid = append(valid, r)
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

// finishFileByPath fsyncs a file whose every article was just read back from
// the device, truncates it to maxEnd if it is larger and maxEnd > 0, fsyncs
// again, and closes it. It never grows a file, and a file no article bounds
// is left alone.
func finishFileByPath(path string, maxEnd int64) (err error) {
	fh, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // G304: the caller's resolver confines path to the job directory
	if err != nil {
		return fmt.Errorf("finish %s: %w", path, err)
	}
	defer func() {
		if cErr := fh.Close(); cErr != nil && err == nil {
			err = fmt.Errorf("finish %s: close: %w", path, cErr)
		}
	}()
	if err := fsutil.ShrinkAndSync(fh, maxEnd, fsyncFile); err != nil {
		return fmt.Errorf("finish %s: %w", path, err)
	}
	return nil
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

// fileFinishable reports whether one file has every article resolved and no
// Complete flag, and so needs finishing. It is the core of that question for
// both strandedComplete and verifyJobFiles, which differ only in where
// "resolved" comes from.
//
// FetchAlways only, matching Job.IsComplete: a deferred or discarded par2
// recovery volume is never dispatched, so "every article resolved" is
// vacuously true of it and completing it would claim a file nobody fetched.
//
// A file with NO articles is excluded for the same reason: the loop below is
// vacuously true over an empty range, so without this an empty file range
// would be reported finishable on every start.
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
