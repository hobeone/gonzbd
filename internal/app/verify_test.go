package app

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/crc32util"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

const verifyArt = 1024

// verifyFixture is one job of one file with four 1 KiB articles, written to
// disk, and a row per article carrying the true CRC of its bytes.
type verifyFixture struct {
	dl, name, path string
	resolve        func(filename string) string // the writer's path for a recorded name
	m              *job.Manifest
	data           []byte
	rows           []durability.WrittenRow
	files          []durability.FileRow
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	arts := make([]job.JobArticle, 4)
	for i := range arts {
		arts[i] = job.JobArticle{ID: fmt.Sprintf("<v%d@t>", i), Bytes: verifyArt, Number: i + 1}
	}
	f := &verifyFixture{
		dl:   t.TempDir(),
		name: "vjob",
		m:    job.NewManifest([]job.JobFile{{Subject: "v.bin", Bytes: 4 * verifyArt, Articles: arts}}),
		data: make([]byte, 4*verifyArt),
	}
	for i := range f.data {
		f.data[i] = byte(i*7 + i/verifyArt + 1)
	}
	if err := os.MkdirAll(filepath.Join(f.dl, f.name), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f.path = filepath.Join(f.dl, f.name, "v.bin")
	f.write(t, f.data)
	for i := range 4 {
		f.rows = append(f.rows, f.rowAt(i, int64(i*verifyArt)))
	}
	f.files = []durability.FileRow{{FileIndex: 0, Filename: "v.bin"}}
	dir := filepath.Dir(f.path)
	f.resolve = func(filename string) string { return filepath.Join(dir, filename) }
	return f
}

func (f *verifyFixture) write(t *testing.T, b []byte) {
	t.Helper()
	if err := os.WriteFile(f.path, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// rowAt is article art's row at off, with the CRC of what is on disk there.
func (f *verifyFixture) rowAt(art int, off int64) durability.WrittenRow {
	return durability.WrittenRow{
		FileIdx: 0, ArtIdx: int32(art), Offset: off, Length: verifyArt, //nolint:gosec // G115: test index
		CRC32: crc32.ChecksumIEEE(f.data[off : off+verifyArt]),
	}
}

func (f *verifyFixture) run(t *testing.T, ctx context.Context, rows []durability.WrittenRow, retry bool) (verifyResult, error) {
	t.Helper()
	return verifyJobFiles(ctx, f.m, f.files, rows, f.resolve, retry)
}

func pick(rows []durability.WrittenRow, idx ...int) []durability.WrittenRow {
	out := make([]durability.WrittenRow, 0, len(idx))
	for _, i := range idx {
		out = append(out, rows[i])
	}
	return out
}

func checkVerifyResult(t *testing.T, got verifyResult, wantVerdicts []durability.FileVerdict,
	wantVerified []durability.WrittenRow, wantFailed []int32, wantFinished []int) {
	t.Helper()
	if !reflect.DeepEqual(got.Verdicts, wantVerdicts) {
		t.Errorf("Verdicts = %+v, want %+v", got.Verdicts, wantVerdicts)
	}
	if !slices.Equal(got.Verified[0], wantVerified) {
		t.Errorf("Verified[0] = %+v, want %+v", got.Verified[0], wantVerified)
	}
	if !slices.Equal(got.Failed[0], wantFailed) {
		t.Errorf("Failed[0] = %v, want %v", got.Failed[0], wantFailed)
	}
	var finished []int
	for _, v := range got.Verdicts {
		if v.SetComplete {
			finished = append(finished, v.FileIdx)
		}
	}
	if !slices.Equal(finished, wantFinished) {
		t.Errorf("files with SetComplete = %v, want %v", finished, wantFinished)
	}
}

// TestVerifyJobFiles_Outcomes pins what each kind of readback does to one
// file: which rows survive as Verified, which are deleted, which articles an
// intersection fails, and when the file is finished by path.
func TestVerifyJobFiles_Outcomes(t *testing.T) {
	t.Parallel()

	type outcome struct {
		verdicts []durability.FileVerdict
		verified []int // indexes into rows
		failed   []int32
		finished []int
	}
	cases := []struct {
		name string
		// prepare edits the fixture on disk and returns the rows to verify.
		prepare func(t *testing.T, f *verifyFixture) []durability.WrittenRow
		retry   bool
		want    outcome
		after   func(t *testing.T, f *verifyFixture)
	}{
		{
			name: "all match, not every article present",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				return pick(f.rows, 0, 1, 2)
			},
			want: outcome{verified: []int{0, 1, 2}},
		},
		{
			name: "one range zeroed on disk",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				b := slices.Clone(f.data)
				clear(b[2*verifyArt : 3*verifyArt])
				f.write(t, b)
				return f.rows
			},
			want: outcome{
				verdicts: []durability.FileVerdict{{FileIdx: 0, DeleteArtIdxs: []int32{2}}},
				verified: []int{0, 1, 3},
			},
		},
		{
			name: "file missing",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				if err := os.Remove(f.path); err != nil {
					t.Fatalf("remove: %v", err)
				}
				return f.rows
			},
			want: outcome{verdicts: []durability.FileVerdict{{FileIdx: 0, DeleteAll: true}}},
		},
		{
			name: "file truncated through the last article",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				f.write(t, f.data[:3*verifyArt+100])
				return f.rows
			},
			want: outcome{
				verdicts: []durability.FileVerdict{{FileIdx: 0, DeleteArtIdxs: []int32{3}}},
				verified: []int{0, 1, 2},
			},
		},
		{
			// Both readbacks match: the second article's row describes the
			// bytes at its offset correctly, but they are the first article's
			// bytes too, so only one of the two can own them.
			name: "two rows intersecting, the first matching",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				return []durability.WrittenRow{f.rows[0], f.rowAt(1, verifyArt/2)}
			},
			want: outcome{
				verdicts: []durability.FileVerdict{{FileIdx: 0, DeleteArtIdxs: []int32{1}}},
				verified: []int{0},
				failed:   []int32{1},
			},
		},
		{
			name: "every article present and matching",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				// Pre-allocation left a tail past the last article.
				f.write(t, append(slices.Clone(f.data), make([]byte, 300)...))
				return f.rows
			},
			want: outcome{
				verdicts: []durability.FileVerdict{{FileIdx: 0, SetComplete: true}},
				verified: []int{0, 1, 2, 3},
				finished: []int{0},
			},
			after: func(t *testing.T, f *verifyFixture) {
				if got := fileSize(t, f.path); got != 4*verifyArt {
					t.Errorf("finished file size = %d, want %d (truncated to maxEnd)", got, 4*verifyArt)
				}
			},
		},
		{
			name: "complete=1 file is not read",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				if os.Geteuid() == 0 {
					t.Skip("chmod 000 does not stop root from reading, so it cannot show no read happened")
				}
				// Zeroed AND unreadable: a read would either fail or delete.
				f.write(t, make([]byte, 4*verifyArt))
				if err := os.Chmod(f.path, 0); err != nil {
					t.Fatalf("chmod: %v", err)
				}
				f.files[0].Complete = true
				return f.rows
			},
			want: outcome{verified: []int{0, 1, 2, 3}},
		},
		{
			name: "complete=0 on-demand file is read",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				f.files[0].FetchPolicy = uint8(job.FetchIfNeeded)
				b := slices.Clone(f.data)
				clear(b[verifyArt : 2*verifyArt])
				f.write(t, b)
				return pick(f.rows, 0, 1)
			},
			want: outcome{
				verdicts: []durability.FileVerdict{{FileIdx: 0, DeleteArtIdxs: []int32{1}}},
				verified: []int{0},
			},
		},
		{
			name: "an empty filename deletes every row",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				f.files[0].Filename = ""
				return f.rows
			},
			want: outcome{verdicts: []durability.FileVerdict{{FileIdx: 0, DeleteAll: true}}},
		},
		{
			// A zero-length row with CRC 0 would "match" the empty read, and a
			// negative offset would fault the job; each costs only its row.
			name: "rows with an impossible range are deleted unread",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				empty := durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Offset: 2 * verifyArt}
				negative := f.rows[3]
				negative.Offset = -1
				return []durability.WrittenRow{f.rows[0], f.rows[1], empty, negative}
			},
			want: outcome{
				verdicts: []durability.FileVerdict{{FileIdx: 0, DeleteArtIdxs: []int32{3, 2}}},
				verified: []int{0, 1},
			},
		},
		{
			name: "rows given out of offset order come back in it",
			prepare: func(t *testing.T, f *verifyFixture) []durability.WrittenRow {
				return pick(f.rows, 2, 0)
			},
			want: outcome{verified: []int{0, 2}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newVerifyFixture(t)
			rows := tc.prepare(t, f)
			res, err := f.run(t, t.Context(), rows, tc.retry)
			if err != nil {
				t.Fatalf("verifyJobFiles: %v", err)
			}
			var wantVerified []durability.WrittenRow
			if tc.want.verified != nil {
				wantVerified = pick(f.rows, tc.want.verified...)
			}
			checkVerifyResult(t, res, tc.want.verdicts, wantVerified, tc.want.failed, tc.want.finished)
			if tc.after != nil {
				tc.after(t, f)
			}
		})
	}
}

// TestVerifyJobFiles_ReadFaultChangesNothing pins Review Focus 1: an EIO in
// the middle of a pass returns a fault naming the file and a zero result, so
// the caller commits nothing and attaches nothing.
//
// Not parallel: it replaces the package-level preadAt seam.
func TestVerifyJobFiles_ReadFaultChangesNothing(t *testing.T) {
	f := newVerifyFixture(t)
	calls := 0
	orig := preadAt
	preadAt = func(fh *os.File, b []byte, off int64) (int, error) {
		calls++
		if calls == 2 {
			return 0, syscall.EIO
		}
		return orig(fh, b, off)
	}
	t.Cleanup(func() { preadAt = orig })

	res, err := f.run(t, t.Context(), f.rows, false)
	assertVerifyFault(t, res, err, f.path)
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("err = %v, want it to wrap EIO", err)
	}
}

// TestVerifyJobFiles_CancelChangesNothing is the same pin for a context
// cancelled after the first row.
//
// Not parallel: it replaces the package-level preadAt seam.
func TestVerifyJobFiles_CancelChangesNothing(t *testing.T) {
	f := newVerifyFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	orig := preadAt
	preadAt = func(fh *os.File, b []byte, off int64) (int, error) {
		n, err := orig(fh, b, off)
		cancel()
		return n, err
	}
	t.Cleanup(func() { preadAt = orig })

	// Not every article, so the finish step's own ctx check cannot catch it.
	res, err := f.run(t, ctx, pick(f.rows, 0, 1, 2), false)
	assertVerifyFault(t, res, err, f.path)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

// TestVerifyJobFiles_OpensTheResolversPath pins that the verifier reads the
// path the caller's resolver returns, not one it derives from the name.
func TestVerifyJobFiles_OpensTheResolversPath(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	dir := filepath.Dir(f.path)
	if err := os.Rename(f.path, filepath.Join(dir, "a_b.bin")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	f.files[0].Filename = "a b.bin"
	f.resolve = func(filename string) string {
		return filepath.Join(dir, strings.ReplaceAll(filename, " ", "_"))
	}
	res, err := f.run(t, t.Context(), pick(f.rows, 0, 1), false)
	if err != nil {
		t.Fatalf("verifyJobFiles: %v", err)
	}
	checkVerifyResult(t, res, nil, pick(f.rows, 0, 1), nil, nil)
}

// TestVerifyJobFiles_MissingDirectoryIsAFault pins that ENOENT is a finding
// about the file only inside a directory that exists: a download root that is
// not mounted must park the job, not delete every row it has.
func TestVerifyJobFiles_MissingDirectoryIsAFault(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	gone := filepath.Join(f.dl, "unmounted", "vjob")
	f.resolve = func(filename string) string { return filepath.Join(gone, filename) }
	res, err := f.run(t, t.Context(), f.rows, false)
	assertVerifyFault(t, res, err, gone)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap the directory's ENOENT", err)
	}
}

// TestVerifyJobFiles_OpenErrorIsAFault pins that only ENOENT is a definitive
// absence: a permission error is a fault, never a DeleteAll.
func TestVerifyJobFiles_OpenErrorIsAFault(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not stop root from opening the file")
	}
	f := newVerifyFixture(t)
	if err := os.Chmod(f.path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	res, err := f.run(t, t.Context(), f.rows, false)
	assertVerifyFault(t, res, err, f.path)
}

// TestVerifyJobFiles_FsyncErrorUntrustsTheFile pins step 3: an fsync error on
// the fresh descriptor deletes every row of the file, and nothing is read.
//
// Not parallel: it replaces the package-level preadAt and fsyncFile seams.
func TestVerifyJobFiles_FsyncErrorUntrustsTheFile(t *testing.T) {
	f := newVerifyFixture(t)
	origSync, origRead := fsyncFile, preadAt
	fsyncFile = func(*os.File) error { return syscall.EIO }
	reads := 0
	preadAt = func(fh *os.File, b []byte, off int64) (int, error) {
		reads++
		return origRead(fh, b, off)
	}
	t.Cleanup(func() { fsyncFile, preadAt = origSync, origRead })

	// Not every article, so nothing would finish the file and fsync there.
	res, err := f.run(t, t.Context(), pick(f.rows, 0, 1, 2), false)
	if err != nil {
		t.Fatalf("verifyJobFiles: %v; an fsync error is a verdict, not a fault", err)
	}
	checkVerifyResult(t, res, []durability.FileVerdict{{FileIdx: 0, DeleteAll: true}}, nil, nil, nil)
	if reads != 0 {
		t.Errorf("%d reads after the fsync failed, want 0", reads)
	}
}

func assertVerifyFault(t *testing.T, res verifyResult, err error, path string) {
	t.Helper()
	var fault *errVerifyFault
	if !errors.As(err, &fault) {
		t.Fatalf("err = %v, want an *errVerifyFault", err)
	}
	if fault.File != path {
		t.Errorf("fault names %q, want %q", fault.File, path)
	}
	if !reflect.DeepEqual(res, verifyResult{}) {
		t.Errorf("result = %+v alongside a fault, want the zero result", res)
	}
}

// TestVerifyJobFiles_RetryDoesNotFinishOverAnIntersectionFailure pins the
// retry rule: ResetForRetry is about to clear an intersection's failure, so a
// retry must not finish the file that failure made finishable.
func TestVerifyJobFiles_RetryDoesNotFinishOverAnIntersectionFailure(t *testing.T) {
	t.Parallel()
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%v", retry), func(t *testing.T) {
			t.Parallel()
			f := newVerifyFixture(t)
			rows := []durability.WrittenRow{f.rows[0], f.rowAt(1, verifyArt/2), f.rows[2], f.rows[3]}
			res, err := f.run(t, t.Context(), rows, retry)
			if err != nil {
				t.Fatalf("verifyJobFiles: %v", err)
			}
			want := []durability.FileVerdict{{FileIdx: 0, DeleteArtIdxs: []int32{1}, SetComplete: !retry}}
			var finished []int
			if !retry {
				finished = []int{0}
			}
			checkVerifyResult(t, res, want, pick(f.rows, 0, 2, 3), []int32{1}, finished)
		})
	}
}

// TestFinishFileByPath pins the bound rule: shrink to maxEnd, never grow, and
// leave a file alone when no article gave it a bound.
func TestFinishFileByPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		size, maxEnd int64
		want         int64
	}{
		{"larger is truncated", 5000, 4096, 4096},
		{"smaller is not grown", 3000, 4096, 3000},
		{"no bound leaves it alone", 5000, 0, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "f.bin")
			if err := os.WriteFile(path, make([]byte, tc.size), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := finishFileByPath(path, tc.maxEnd); err != nil {
				t.Fatalf("finishFileByPath: %v", err)
			}
			if got := fileSize(t, path); got != tc.want {
				t.Errorf("size = %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("a failing second fsync is returned", func(t *testing.T) {
		// Not parallel: it replaces the package-level fsyncFile seam.
		path := filepath.Join(t.TempDir(), "f.bin")
		if err := os.WriteFile(path, make([]byte, 5000), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		orig := fsyncFile
		t.Cleanup(func() { fsyncFile = orig })
		calls := 0
		fsyncFile = func(f *os.File) error {
			calls++
			if calls == 2 {
				return syscall.EIO
			}
			return orig(f)
		}
		err := finishFileByPath(path, 4096)
		if !errors.Is(err, syscall.EIO) {
			t.Errorf("err = %v, want one wrapping EIO from the second fsync", err)
		}
		if calls != 2 {
			t.Errorf("fsynced %d times, want 2", calls)
		}
	})
	t.Run("a missing file is an error naming it", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "gone.bin")
		err := finishFileByPath(path, 10)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("err = %v, want an error naming %s", err, path)
		}
	})
}

// TestFileCRCFromRows pins the derivation's single-chain predicate: a CRC
// only from a gapless chain of every article of the file, starting at 0, with
// none failed.
func TestFileCRCFromRows(t *testing.T) {
	t.Parallel()
	data := make([]byte, 4*verifyArt)
	for i := range data {
		data[i] = byte(i*13 + 5)
	}
	// File 1 of a job whose file 0 has 3 articles, so lo is not 0.
	const lo, hi = 3, 7
	chain := func() []durability.WrittenRow {
		rows := make([]durability.WrittenRow, 0, 4)
		for i := range 4 {
			off := int64(i * verifyArt)
			rows = append(rows, durability.WrittenRow{
				FileIdx: 1, ArtIdx: int32(lo + i), Offset: off, Length: verifyArt, //nolint:gosec // G115: test index
				CRC32: crc32.ChecksumIEEE(data[off : off+verifyArt]),
			})
		}
		return rows
	}

	got, ok := fileCRCFromRows(chain(), false, lo, hi)
	if !ok || got != crc32.ChecksumIEEE(data) {
		t.Errorf("gapless chain = %08x, %v; want %08x, true", got, ok, crc32.ChecksumIEEE(data))
	}
	// The same chain as file 0 of a job, where the range starts at 0.
	first := chain()
	for i := range first {
		first[i].FileIdx, first[i].ArtIdx = 0, int32(i) //nolint:gosec // G115: test index
	}
	if got, ok := fileCRCFromRows(first, false, 0, 4); !ok || got != crc32.ChecksumIEEE(data) {
		t.Errorf("gapless chain at lo=0 = %08x, %v; want %08x, true", got, ok, crc32.ChecksumIEEE(data))
	}

	for name, mutate := range map[string]func([]durability.WrittenRow) []durability.WrittenRow{
		"a gap": func(r []durability.WrittenRow) []durability.WrittenRow {
			r[2].Offset++
			r[3].Offset++
			return r
		},
		"a straddle": func(r []durability.WrittenRow) []durability.WrittenRow {
			r[1].Offset = verifyArt / 2
			return r
		},
		"first row not at 0": func(r []durability.WrittenRow) []durability.WrittenRow {
			for i := range r {
				r[i].Offset += 10
			}
			return r
		},
		"a missing article": func(r []durability.WrittenRow) []durability.WrittenRow {
			return r[:3]
		},
		"a duplicated article": func(r []durability.WrittenRow) []durability.WrittenRow {
			r[1].ArtIdx = r[0].ArtIdx
			return r
		},
		"an article outside the range": func(r []durability.WrittenRow) []durability.WrittenRow {
			r[3].ArtIdx = hi
			return r
		},
	} {
		if _, ok := fileCRCFromRows(mutate(chain()), false, lo, hi); ok {
			t.Errorf("%s: fileCRCFromRows returned a CRC, want NoCRC", name)
		}
	}
	if _, ok := fileCRCFromRows(chain(), true, lo, hi); ok {
		t.Error("failed=true: fileCRCFromRows returned a CRC, want NoCRC")
	}
	if _, ok := fileCRCFromRows(nil, false, lo, lo); ok {
		t.Error("an empty range: fileCRCFromRows returned a CRC, want NoCRC")
	}
	// Combine is what the chain uses; pin the two agree on a two-row split.
	if c := crc32util.Combine(crc32.ChecksumIEEE(data[:10]), crc32.ChecksumIEEE(data[10:20]), 10); c != crc32.ChecksumIEEE(data[:20]) {
		t.Fatalf("fixture guard: Combine disagrees with ChecksumIEEE")
	}
}

// TestFileFinishable pins each exclusion of the shared "needs finishing" rule
// that strandedComplete and verifyJobFiles both call.
func TestFileFinishable(t *testing.T) {
	t.Parallel()
	m := job.NewManifest([]job.JobFile{
		{Subject: "a.bin", Bytes: 2, Articles: []job.JobArticle{{ID: "<a0@t>", Bytes: 1}, {ID: "<a1@t>", Bytes: 1}}},
		{Subject: "empty.bin"},
	})
	all := func(int) bool { return true }
	if !fileFinishable(m, 0, job.FetchAlways, false, all) {
		t.Fatal("fixture guard: a fully resolved FetchAlways file is not finishable, so every negative below passes for the wrong reason")
	}
	for name, got := range map[string]bool{
		"an unresolved article": fileFinishable(m, 0, job.FetchAlways, false, func(i int) bool { return i != 1 }),
		"already complete":      fileFinishable(m, 0, job.FetchAlways, true, all),
		"an on-demand volume":   fileFinishable(m, 0, job.FetchIfNeeded, false, all),
		"a discarded volume":    fileFinishable(m, 0, job.FetchNever, false, all),
		"a file of no articles": fileFinishable(m, 1, job.FetchAlways, false, all),
		"an out-of-range file":  fileFinishable(m, 2, job.FetchAlways, false, all),
		"a negative file":       fileFinishable(m, -1, job.FetchAlways, false, all),
		"no manifest":           fileFinishable(nil, 0, job.FetchAlways, false, all),
	} {
		if got {
			t.Errorf("%s: reported finishable", name)
		}
	}
}

// TestResolveRows pins intersection resolution: a matching row keeps its
// range, and whatever intersects a kept row is failed whether or not it
// matched itself.
func TestResolveRows(t *testing.T) {
	t.Parallel()
	a := durability.WrittenRow{ArtIdx: 0, Offset: 0, Length: 100}
	b := durability.WrittenRow{ArtIdx: 1, Offset: 50, Length: 100}
	c := durability.WrittenRow{ArtIdx: 2, Offset: 200, Length: 100}
	for _, tc := range []struct {
		name     string
		match    []bool
		verified []durability.WrittenRow
		failed   []int32
		deleted  []int32
	}{
		{"both match: the first wins", []bool{true, true, true}, []durability.WrittenRow{a, c}, []int32{1}, []int32{1}},
		{"only the second matches: it wins", []bool{false, true, true}, []durability.WrittenRow{b, c}, []int32{0}, []int32{0}},
		{"neither matches: both are Outstanding", []bool{false, false, true}, []durability.WrittenRow{c}, nil, []int32{0, 1}},
		{"a lone mismatch is deleted, not failed", []bool{true, true, false}, []durability.WrittenRow{a}, []int32{1}, []int32{1, 2}},
	} {
		got := resolveRows([]durability.WrittenRow{a, b, c}, tc.match)
		if !slices.Equal(got.verified, tc.verified) || !slices.Equal(got.failed, tc.failed) ||
			!slices.Equal(got.deleted, tc.deleted) || got.deleteAll {
			t.Errorf("%s: got verified=%v failed=%v deleted=%v deleteAll=%v; want verified=%v failed=%v deleted=%v",
				tc.name, got.verified, got.failed, got.deleted, got.deleteAll, tc.verified, tc.failed, tc.deleted)
		}
	}
}

// TestResolveRows_ALongRowReachesAKeptRowAfterIt pins the lookahead: a
// non-matching row that overlaps only the kept row that FOLLOWS it in offset
// order is failed, not merely deleted.
func TestResolveRows_ALongRowReachesAKeptRowAfterIt(t *testing.T) {
	t.Parallel()
	a := durability.WrittenRow{ArtIdx: 0, Offset: 0, Length: 100}
	long := durability.WrittenRow{ArtIdx: 1, Offset: 120, Length: 200}
	c := durability.WrittenRow{ArtIdx: 2, Offset: 200, Length: 100}
	got := resolveRows([]durability.WrittenRow{a, long, c}, []bool{true, false, true})
	if !slices.Equal(got.verified, []durability.WrittenRow{a, c}) ||
		!slices.Equal(got.failed, []int32{1}) || !slices.Equal(got.deleted, []int32{1}) {
		t.Errorf("got verified=%v failed=%v deleted=%v; want a and c kept, art 1 failed and deleted",
			got.verified, got.failed, got.deleted)
	}
}

// TestReadBackFile_ReadsARowLongerThanTheBuffer pins the chunked read: a row
// longer than the buffer is hashed across several reads, not truncated to
// the first. A missing file is a definitive deleteAll, not an error.
func TestReadBackFile_ReadsARowLongerThanTheBuffer(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	whole := durability.WrittenRow{Offset: 0, Length: int64(len(f.data)), CRC32: crc32.ChecksumIEEE(f.data)}
	out, err := readBackFile(t.Context(), f.path, []durability.WrittenRow{whole}, make([]byte, 1000))
	if err != nil {
		t.Fatalf("readBackFile: %v", err)
	}
	if !slices.Equal(out.verified, []durability.WrittenRow{whole}) {
		t.Errorf("verified = %+v, want the one row: its CRC spans several buffer fills", out.verified)
	}

	gone, err := readBackFile(t.Context(), f.path+".missing", []durability.WrittenRow{whole}, make([]byte, 1000))
	if err != nil || !gone.deleteAll {
		t.Errorf("missing file = %+v, %v; want deleteAll and no error", gone, err)
	}
}

// TestFinishIfResolved pins what stops a read-back file from being finished:
// an untrusted file, a file nothing verified, and a cancelled context or a
// failed finish, which are faults rather than a "no".
func TestFinishIfResolved(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	fr := f.files[0]
	every := fileReadback{verified: f.rows}

	if ok, err := finishIfResolved(t.Context(), f.m, fr, f.path, fileReadback{deleteAll: true}, false); ok || err != nil {
		t.Errorf("untrusted file: finished=%v err=%v, want neither", ok, err)
	}
	if ok, err := finishIfResolved(t.Context(), f.m, fr, f.path, fileReadback{}, false); ok || err != nil {
		t.Errorf("nothing verified: finished=%v err=%v, want neither", ok, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ok, err := finishIfResolved(ctx, f.m, fr, f.path, every, false); ok || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: finished=%v err=%v, want a context error", ok, err)
	}
	if ok, err := finishIfResolved(t.Context(), f.m, fr, f.path+".missing", every, false); ok || err == nil {
		t.Errorf("finish fails: finished=%v err=%v, want the error", ok, err)
	}
	if ok, err := finishIfResolved(t.Context(), f.m, fr, f.path, every, false); !ok || err != nil {
		t.Errorf("every article verified: finished=%v err=%v, want finished", ok, err)
	}
}
