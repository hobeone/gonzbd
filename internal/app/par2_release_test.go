package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/types"
)

// The par2 fixture (test/fixtures/par2/data.par2) protects data.bin, 4065
// bytes in 4-byte slices. The jobs below split data.bin into three articles
// around one 40-byte interior article, so losing that article damages exactly
// ten slices (2000/4 .. 2039/4) and a release must cover ten blocks.
const (
	fixtureSliceDamage = 10
	artHead            = 2000
	artMid             = 40
	artTail            = 4065 - artHead - artMid
)

// Three recovery volumes with real "+MM" names. The cheapest cover of ten
// blocks is the first two (12 blocks, 3000 bytes); the third alone (16 blocks)
// costs 4000.
var sizedVols = []sizedVol{
	{"data.vol000+04.par2", 1000},
	{"data.vol004+08.par2", 2000},
	{"data.vol012+16.par2", 4000},
}

type sizedVol struct {
	name  string
	bytes int64
}

// newSizedJob builds a not-yet-added job with on-demand par2 on: data.bin in
// articles of the given sizes, and one single-article file per volume.
func newSizedJob(t *testing.T, id, name string, pp int, arts []int, vols []sizedVol) (*job.Job, dispatch.Header) {
	t.Helper()
	return newSizedJobAfter(t, id, name, pp, nil, arts, vols)
}

// newSizedJobAfter is newSizedJob with the files of lead placed before
// data.bin in the manifest.
func newSizedJobAfter(t *testing.T, id, name string, pp int, lead []nzb.File, arts []int, vols []sizedVol) (*job.Job, dispatch.Header) {
	t.Helper()
	data := nzb.File{Subject: "data.bin"}
	for i, b := range arts {
		data.Articles = append(data.Articles, nzb.Article{ID: fmt.Sprintf("d%d@t", i), Bytes: b, Number: i + 1})
		data.Bytes += int64(b)
	}
	parsed := &nzb.NZB{Files: append(slices.Clone(lead), data)}
	for i, v := range vols {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject: v.name, Bytes: v.bytes,
			Articles: []nzb.Article{{ID: fmt.Sprintf("v%d@t", i), Bytes: int(v.bytes), Number: 1}},
		})
	}
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.Downloads.OnDemandPar2 = true
	j, hdr, err := BuildIngestJob(cfg, parsed, "t.nzb", types.FetchOptions{JobID: id, NzbName: name, PP: pp}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j, hdr
}

// fileIdxBySubject returns the manifest index of the file with that subject.
func fileIdxBySubject(t *testing.T, j *job.Job, subject string) int {
	t.Helper()
	m := mustManifest(t, j)
	for fi := range m.NumFiles() {
		if m.Files().Subject(fi) == subject {
			return fi
		}
	}
	t.Fatalf("no file with subject %q", subject)
	return -1
}

// rowsExcept returns the written rows of data file dataIdx, each article
// tiling the file end to end, less the articles whose file-local index is in
// missing.
func rowsExcept(t *testing.T, j *job.Job, dataIdx int, missing ...int) []durability.WrittenRow {
	t.Helper()
	m := mustManifest(t, j)
	lo, hi := m.FileRange(dataIdx)
	var rows []durability.WrittenRow
	var off int64
	for a := lo; a < hi; a++ {
		n := int64(m.ArticleBytes(a))
		if !slices.Contains(missing, a-lo) {
			rows = append(rows, durability.WrittenRow{FileIdx: dataIdx, ArtIdx: int32(a), Offset: off, Length: n}) //nolint:gosec // G115: article counts are far below int32
		}
		off += n
	}
	return rows
}

// assessDir assesses dir against j the way maybeReleaseRecoveryVolumes does.
func assessDir(t *testing.T, dir string, j *job.Job) par2.Assessment {
	t.Helper()
	opts := par2.DefaultParseOptions()
	sets, err := par2.FindPar2Files(dir, opts)
	if err != nil || len(sets) == 0 {
		t.Fatalf("FindPar2Files: %v (%d sets)", err, len(sets))
	}
	a, err := par2.AssessWithOptions(dir, sets, assembledFiles(mustManifest(t, j).Files(), j.Progress()),
		slog.New(slog.DiscardHandler), opts)
	if err != nil {
		t.Fatalf("AssessWithOptions: %v", err)
	}
	return a
}

// rowSource is a loadRows func that counts its calls.
type rowSource struct {
	rows  []durability.WrittenRow
	err   error
	calls int
}

func (s *rowSource) load() ([]durability.WrittenRow, error) {
	s.calls++
	return s.rows, s.err
}

// verifySource is a verify func that counts its calls.
type verifySource struct {
	res   par2.VerifyResult
	err   error
	calls int
}

func (v *verifySource) run() (par2.VerifyResult, error) {
	v.calls++
	return v.res, v.err
}

// noVerify is a verify func for a case that must not reach one.
func noVerify() (par2.VerifyResult, error) {
	return par2.VerifyResult{}, errors.New("verify must not run")
}

// fixtureDataBlocks is data.bin's block count at the fixture's 4-byte slices.
const fixtureDataBlocks = (4065 + 3) / 4

func TestRecoveryDeficit(t *testing.T) {
	t.Parallel()

	// build returns a fresh job and a directory holding the fixture's index and
	// data.bin, and the data file's index. The data.bin job is complete but for
	// the interior article.
	build := func(t *testing.T) (*job.Job, string, int) {
		t.Helper()
		j, _ := newSizedJob(t, "deficit", "deficit", types.PPRepair, []int{artHead, artMid, artTail}, sizedVols)
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		copyFixturePayload(t, dir, "data.bin")
		return j, dir, fileIdxBySubject(t, j, "data.bin")
	}
	release := func(t *testing.T, j *job.Job, subject string) int {
		t.Helper()
		fi := fileIdxBySubject(t, j, subject)
		if err := j.SetFileFetchPolicy(fi, job.FetchAlways); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		if err := j.MarkFileComplete(fi); err != nil {
			t.Fatalf("MarkFileComplete: %v", err)
		}
		return fi
	}

	tests := []struct {
		name      string
		prep      func(t *testing.T, j *job.Job, dir string) // before the assessment
		tweak     func(a *par2.Assessment)                   // after the assessment
		src       func(t *testing.T, j *job.Job, dataIdx int) *rowSource
		want      job.RecoverySizing
		wantWhy   string // substring; empty means no reason
		wantCalls int
	}{
		{
			name: "one interior article missing is sized by the damaged slices",
			want: job.NeedBlocks(fixtureSliceDamage), wantCalls: 1,
		},
		{
			name: "a fully written file needs nothing",
			src: func(t *testing.T, j *job.Job, dataIdx int) *rowSource {
				return &rowSource{rows: rowsExcept(t, j, dataIdx)}
			},
			want: job.NeedBlocks(0), wantCalls: 1,
		},
		{
			name: "a fetched intact volume is subtracted",
			prep: func(t *testing.T, j *job.Job, _ string) { release(t, j, "data.vol000+04.par2") },
			want: job.NeedBlocks(fixtureSliceDamage - 4), wantCalls: 1,
		},
		{
			name: "a fetched volume with a failed article contributes nothing",
			prep: func(t *testing.T, j *job.Job, _ string) {
				fi := release(t, j, "data.vol000+04.par2")
				lo, _ := mustManifest(t, j).FileRange(fi)
				if err := j.MarkArticleFailed(lo, job.WholeArticle); err != nil {
					t.Fatalf("MarkArticleFailed: %v", err)
				}
			},
			want: job.NeedBlocks(fixtureSliceDamage), wantCalls: 1,
		},
		{
			name: "an unreadable written record is unsized",
			src: func(*testing.T, *job.Job, int) *rowSource {
				return &rowSource{err: errors.New("disk on fire")}
			},
			want: job.Unsized(), wantWhy: "disk on fire", wantCalls: 1,
		},
		{
			name: "more slices than a par2 set can hold is unsized without reading rows",
			tweak: func(a *par2.Assessment) {
				a.ID.Files = slices.Clone(a.ID.Files)
				a.ID.Files[0].Desc.FileSize = 4 * (job.MaxPar2Slices + 1)
			},
			want: job.Unsized(), wantWhy: "more than", wantCalls: 0,
		},
		{
			name: "exactly the slice limit is sized",
			tweak: func(a *par2.Assessment) {
				a.ID.Files = slices.Clone(a.ID.Files)
				a.ID.Files[0].Desc.FileSize = 4 * job.MaxPar2Slices
			},
			// The rows cover 4065 of the 131072 bytes, so every slice from
			// the one holding byte 4065 (slice 1016) to the last is damaged
			// as well as the ten around the lost article.
			want: job.NeedBlocks(fixtureSliceDamage + job.MaxPar2Slices - 1016), wantCalls: 1,
		},
		{
			name: "a file size beyond int64 is unsized",
			tweak: func(a *par2.Assessment) {
				a.ID.Files = slices.Clone(a.ID.Files)
				a.ID.Files[0].Desc.FileSize = math.MaxInt64 + 1
			},
			want: job.Unsized(), wantWhy: "out of range", wantCalls: 0,
		},
		{
			name: "an entry with no delivered file is unsized",
			tweak: func(a *par2.Assessment) {
				a.ID.Files = slices.Clone(a.ID.Files)
				a.ID.Files[0].OnDisk = "not-delivered.bin"
			},
			want: job.Unsized(), wantWhy: "no delivered file", wantCalls: 0,
		},
		{
			name: "a second set that reads as nothing is unsized without reading rows",
			prep: func(t *testing.T, _ *job.Job, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "other.par2"), make([]byte, 4096), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: job.Unsized(), wantWhy: "describes no file", wantCalls: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j, dir, dataIdx := build(t)
			if tc.prep != nil {
				tc.prep(t, j, dir)
			}
			a := assessDir(t, dir, j)
			if tc.tweak != nil {
				tc.tweak(&a)
			}
			src := &rowSource{rows: rowsExcept(t, j, dataIdx, 1)}
			if tc.src != nil {
				src = tc.src(t, j, dataIdx)
			}
			ver := &verifySource{err: errors.New("verify must not run on the gap path")}
			got, why := recoveryDeficit(mustManifest(t, j), j.Progress(), a, src.load, ver.run)
			if ver.calls != 0 {
				t.Errorf("verify called %d times, want 0: nothing in the assessment needs a block-level check", ver.calls)
			}
			if got != tc.want {
				t.Errorf("sizing = %+v, want %+v (why %q)", got, tc.want, why)
			}
			if (tc.wantWhy == "") != (why == "") || !strings.Contains(why, tc.wantWhy) {
				t.Errorf("why = %q, want it to contain %q", why, tc.wantWhy)
			}
			if src.calls != tc.wantCalls {
				t.Errorf("loadRows called %d times, want %d", src.calls, tc.wantCalls)
			}
		})
	}

	// Damage the written rows do not show: a file whose assembled CRC differs
	// from par2's, or an entry that could not be checked, is sized by a
	// block-level verify.
	mismatchJob := func(t *testing.T) (*job.Job, string, int) {
		t.Helper()
		j, dir, dataIdx := build(t)
		seedFileCRC(t, j, dataIdx, 0xDEADBEEF)
		return j, dir, dataIdx
	}
	unverifiedJob := func(t *testing.T) (*job.Job, string, int) {
		t.Helper()
		j, dir, dataIdx := build(t)
		if err := os.Remove(filepath.Join(dir, "data.bin")); err != nil {
			t.Fatal(err)
		}
		return j, dir, dataIdx
	}
	verifiedTests := []struct {
		name      string
		mk        func(t *testing.T) (*job.Job, string, int)
		prep      func(t *testing.T, j *job.Job)
		tweak     func(a *par2.Assessment)
		ver       par2.VerifyResult
		verErr    error
		rows      func(t *testing.T, j *job.Job, dataIdx int) *rowSource
		want      job.RecoverySizing
		wantWhy   string
		wantVerif int
	}{
		{
			name: "a mismatch the recovery on disk covers needs nothing",
			mk:   mismatchJob, ver: par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 0},
			want: job.NeedBlocks(0), wantVerif: 1,
		},
		{
			name: "a mismatch needing one block",
			mk:   mismatchJob, ver: par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 1},
			want: job.NeedBlocks(1), wantVerif: 1,
		},
		{
			name: "a mismatch needing seven blocks",
			mk:   mismatchJob, ver: par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 7},
			want: job.NeedBlocks(7), wantVerif: 1,
		},
		{
			name: "an unchecked entry is sized by the verify",
			mk:   unverifiedJob, ver: par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: fixtureDataBlocks},
			want: job.NeedBlocks(fixtureDataBlocks), wantVerif: 1,
		},
		{
			name: "the gaps in the written rows win over a smaller verify",
			mk:   mismatchJob, ver: par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 3},
			rows: func(t *testing.T, j *job.Job, dataIdx int) *rowSource {
				return &rowSource{rows: rowsExcept(t, j, dataIdx, 1)}
			},
			want: job.NeedBlocks(fixtureSliceDamage), wantVerif: 1,
		},
		{
			name: "the verify is already net of a volume fetched intact, which is not subtracted again",
			mk:   mismatchJob,
			prep: func(t *testing.T, j *job.Job) { release(t, j, "data.vol000+04.par2") },
			ver:  par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 7},
			want: job.NeedBlocks(7), wantVerif: 1,
		},
		{
			name: "a larger verify wins over the gaps in the written rows",
			mk:   mismatchJob, ver: par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 12},
			rows: func(t *testing.T, j *job.Job, dataIdx int) *rowSource {
				return &rowSource{rows: rowsExcept(t, j, dataIdx, 1)}
			},
			want: job.NeedBlocks(12), wantVerif: 1,
		},
		{
			name: "unreadable written rows leave the verify alone",
			mk:   mismatchJob, ver: par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 5},
			rows: func(*testing.T, *job.Job, int) *rowSource { return &rowSource{err: errors.New("disk on fire")} },
			want: job.NeedBlocks(5), wantVerif: 1,
		},
		{
			name: "a verify that counts fewer data blocks than the index describes is unsized",
			mk:   mismatchJob, ver: par2.VerifyResult{DataBlocks: fixtureDataBlocks - 1, BlocksNeeded: 1},
			want: job.Unsized(), wantWhy: "data blocks", wantVerif: 1,
		},
		{
			name: "a verify that counts more data blocks than the index describes is unsized",
			mk:   mismatchJob, ver: par2.VerifyResult{DataBlocks: fixtureDataBlocks + 1, BlocksNeeded: 1},
			want: job.Unsized(), wantWhy: "data blocks", wantVerif: 1,
		},
		{
			name: "an entry nothing delivered matched is counted among the data blocks",
			mk:   mismatchJob,
			tweak: func(a *par2.Assessment) {
				a.ID.Unaccounted = []par2.FileDesc{{FileName: "gone.bin", FileSize: 40}}
			},
			ver:  par2.VerifyResult{DataBlocks: fixtureDataBlocks + 10, BlocksNeeded: 10},
			want: job.NeedBlocks(10), wantVerif: 1,
		},
		{
			name: "an entry nothing delivered matched is not left out of the data blocks",
			mk:   mismatchJob,
			tweak: func(a *par2.Assessment) {
				a.ID.Unaccounted = []par2.FileDesc{{FileName: "gone.bin", FileSize: 40}}
			},
			ver:  par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 10},
			want: job.Unsized(), wantWhy: "data blocks", wantVerif: 1,
		},
		{
			name: "an entry no File Description describes is unsized without a verify",
			mk:   mismatchJob,
			tweak: func(a *par2.Assessment) {
				a.ID.Unaccounted = []par2.FileDesc{{FileName: "lost.bin", Undescribed: true}}
			},
			ver:  par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 1},
			want: job.Unsized(), wantWhy: "no file description",
		},
		{
			name: "an identified entry with no delivered file is sized by the verify alone",
			mk:   mismatchJob,
			tweak: func(a *par2.Assessment) {
				a.ID.Files = slices.Clone(a.ID.Files)
				a.ID.Files[0].OnDisk = "not-delivered.bin"
			},
			ver:  par2.VerifyResult{DataBlocks: fixtureDataBlocks, BlocksNeeded: 2},
			want: job.NeedBlocks(2), wantVerif: 1,
		},
		{
			name: "a failed verify is unsized",
			mk:   mismatchJob, verErr: errors.New("engine panic"),
			want: job.Unsized(), wantWhy: "engine panic", wantVerif: 1,
		},
		{
			name: "more slices than a par2 set can hold is unsized without a verify",
			mk:   mismatchJob,
			tweak: func(a *par2.Assessment) {
				a.ID.Files = slices.Clone(a.ID.Files)
				a.ID.Files[0].Desc.FileSize = 4 * (job.MaxPar2Slices + 1)
			},
			want: job.Unsized(), wantWhy: "more than",
		},
	}
	for _, tc := range verifiedTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j, dir, dataIdx := tc.mk(t)
			if tc.prep != nil {
				tc.prep(t, j)
			}
			a := assessDir(t, dir, j)
			if a.CRC.Mismatched == 0 && a.CRC.Unverified == 0 {
				t.Fatalf("fixture guard: the assessment shows no damage the rows cannot: %+v", a.CRC)
			}
			if tc.tweak != nil {
				tc.tweak(&a)
			}
			src := &rowSource{rows: rowsExcept(t, j, dataIdx)}
			if tc.rows != nil {
				src = tc.rows(t, j, dataIdx)
			}
			ver := &verifySource{res: tc.ver, err: tc.verErr}
			got, why := recoveryDeficit(mustManifest(t, j), j.Progress(), a, src.load, ver.run)
			if got != tc.want {
				t.Errorf("sizing = %+v, want %+v (why %q)", got, tc.want, why)
			}
			if (tc.wantWhy == "") != (why == "") || !strings.Contains(why, tc.wantWhy) {
				t.Errorf("why = %q, want it to contain %q", why, tc.wantWhy)
			}
			if ver.calls != tc.wantVerif {
				t.Errorf("verify called %d times, want %d", ver.calls, tc.wantVerif)
			}
		})
	}

	// The same damage, sized by the real engine.
	realVerify := func(dir string) func() (par2.VerifyResult, error) {
		return func() (par2.VerifyResult, error) {
			sets, err := par2.FindPar2Files(dir, par2.DefaultParseOptions())
			if err != nil || len(sets) != 1 {
				return par2.VerifyResult{}, fmt.Errorf("sets %d: %w", len(sets), err)
			}
			cands, err := par2.CandidateFiles(dir)
			if err != nil {
				return par2.VerifyResult{}, err
			}
			return par2.GoVerify(t.Context(), slog.New(slog.DiscardHandler), sets[0].ParseFile(), cands, nil)
		}
	}
	t.Run("a mismatch on a file that is intact on disk needs no block", func(t *testing.T) {
		t.Parallel()
		j, dir, dataIdx := mismatchJob(t)
		a := assessDir(t, dir, j)
		src := &rowSource{rows: rowsExcept(t, j, dataIdx)}
		got, why := recoveryDeficit(mustManifest(t, j), j.Progress(), a, src.load, realVerify(dir))
		if want := job.NeedBlocks(0); got != want || why != "" {
			t.Errorf("sizing = %+v, %q; want %+v with no reason", got, why, want)
		}
	})
	t.Run("a file missing from disk needs all its blocks", func(t *testing.T) {
		t.Parallel()
		j, dir, dataIdx := unverifiedJob(t)
		a := assessDir(t, dir, j)
		src := &rowSource{rows: rowsExcept(t, j, dataIdx)}
		got, why := recoveryDeficit(mustManifest(t, j), j.Progress(), a, src.load, realVerify(dir))
		if want := job.NeedBlocks(fixtureDataBlocks); got != want || why != "" {
			t.Errorf("sizing = %+v, %q; want %+v with no reason", got, why, want)
		}
	})
	t.Run("a file truncated by a lost tail article needs the blocks it lost", func(t *testing.T) {
		t.Parallel()
		j, dir, dataIdx := mismatchJob(t)
		b, err := os.ReadFile(filepath.Join(dir, "data.bin"))
		if err != nil {
			t.Fatal(err)
		}
		const kept = artHead + artMid
		if err := os.WriteFile(filepath.Join(dir, "data.bin"), b[:kept], 0o600); err != nil {
			t.Fatal(err)
		}
		a := assessDir(t, dir, j)
		src := &rowSource{rows: rowsExcept(t, j, dataIdx, 2)}
		got, why := recoveryDeficit(mustManifest(t, j), j.Progress(), a, src.load, realVerify(dir))
		if want := job.NeedBlocks(fixtureDataBlocks - kept/4); got != want || why != "" {
			t.Errorf("sizing = %+v, %q; want %+v with no reason", got, why, want)
		}
	})

	// The par2 index protects data.bin only, so its identification index is 0
	// while pad.bin, which par2 does not protect, holds manifest index 0. The
	// written rows are keyed by manifest index.
	t.Run("an identified file's rows are read by its manifest index", func(t *testing.T) {
		t.Parallel()
		pad := nzb.File{Subject: "pad.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "p0@t", Bytes: 1000, Number: 1}}}
		j, _ := newSizedJobAfter(t, "order", "order", types.PPRepair, []nzb.File{pad}, []int{artHead, artMid, artTail}, sizedVols)
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		copyFixturePayload(t, dir, "data.bin")
		padIdx, dataIdx := fileIdxBySubject(t, j, "pad.bin"), fileIdxBySubject(t, j, "data.bin")
		a := assessDir(t, dir, j)
		if padIdx != 0 || dataIdx != 1 || len(a.ID.Files) != 1 || a.ID.Files[0].OnDisk != "data.bin" {
			t.Fatalf("fixture guard: pad.bin at %d, data.bin at %d, identified %+v; want 0, 1 and data.bin alone",
				padIdx, dataIdx, a.ID.Files)
		}
		src := &rowSource{rows: append(rowsExcept(t, j, padIdx), rowsExcept(t, j, dataIdx, 1)...)}
		got, why := recoveryDeficit(mustManifest(t, j), j.Progress(), a, src.load, noVerify)
		if want := job.NeedBlocks(fixtureSliceDamage); got != want || why != "" {
			t.Errorf("sizing = %+v, %q; want %+v with no reason", got, why, want)
		}
	})

	t.Run("a minus-form volume fetched whole contributes nothing", func(t *testing.T) {
		t.Parallel()
		vols := []sizedVol{{"data.vol000-07.par2", 1000}, {"data.vol004+08.par2", 2000}}
		j, _ := newSizedJob(t, "minus", "minus", types.PPRepair, []int{artHead, artMid, artTail}, vols)
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		copyFixturePayload(t, dir, "data.bin")
		release(t, j, "data.vol000-07.par2")
		dataIdx := fileIdxBySubject(t, j, "data.bin")
		src := &rowSource{rows: rowsExcept(t, j, dataIdx, 1)}
		got, why := recoveryDeficit(mustManifest(t, j), j.Progress(), assessDir(t, dir, j), src.load, noVerify)
		if want := job.NeedBlocks(fixtureSliceDamage); got != want || why != "" {
			t.Errorf("sizing = %+v, %q; want %+v with no reason: a -NN name declares a volume number, not a block count", got, why, want)
		}
	})

	t.Run("two par2 sets give no single slice size", func(t *testing.T) {
		t.Parallel()
		bin, err := exec.LookPath("par2")
		if err != nil {
			t.Skipf("par2 unavailable to build a second set: %v", err)
		}
		// A second, fully accounted set: other.bin and its own index, with a
		// different slice size from the fixture's.
		j, _ := newSizedJob(t, "twosets", "twosets", types.PPRepair, []int{artHead, artMid, artTail},
			append(slices.Clone(sizedVols), sizedVol{"other.bin", 100}))
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		copyFixturePayload(t, dir, "data.bin")
		if err := os.WriteFile(filepath.Join(dir, "other.bin"), make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), bin, "c", "-q", "-s8", "-r1", "-n1", "other.par2", "other.bin")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("par2 create: %v\n%s", err, out)
		}
		dataIdx := fileIdxBySubject(t, j, "data.bin")
		a := assessDir(t, dir, j)
		if _, ok := a.ID.SliceSize(); ok {
			t.Fatalf("fixture guard: the two indexes did not parse as two sets")
		}
		if a.CRC.Mismatched != 0 || a.CRC.Unverified != 0 {
			t.Fatalf("fixture guard: a finding would be reported before the slice size is read: %+v", a.CRC)
		}
		src := &rowSource{rows: rowsExcept(t, j, dataIdx, 1)}
		got, why := recoveryDeficit(mustManifest(t, j), j.Progress(), a, src.load, noVerify)
		if got != job.Unsized() || !strings.Contains(why, "slice size") {
			t.Errorf("sizing = %+v, %q; want Unsized because no single set gives a slice size", got, why)
		}
		if src.calls != 0 {
			t.Errorf("loadRows called %d times, want 0", src.calls)
		}
	})
}

// deficitRowsStore serves a fixed set of written rows over the app's store.
type deficitRowsStore struct {
	durabilityStore
	rows []durability.WrittenRow
}

func (s *deficitRowsStore) WrittenRows(context.Context, string) ([]durability.WrittenRow, error) {
	return s.rows, nil
}

// cancellingRowsStore cancels the assessment's ctx from inside the rows read,
// as a shutdown arriving mid-assessment does, and fails the read with it.
type cancellingRowsStore struct {
	durabilityStore
	cancel context.CancelFunc
}

func (s *cancellingRowsStore) WrittenRows(ctx context.Context, _ string) ([]durability.WrittenRow, error) {
	s.cancel()
	return nil, ctx.Err()
}

// releaseApp returns an application with on-demand par2 on and a download
// directory, plus the job's directory prepared with the fixture index and
// payload.
func releaseApp(t *testing.T, jobName string) (*Application, string) {
	t.Helper()
	dir := t.TempDir()
	application := newTestApplication(t)
	application.config.With(func(c *config.Config) {
		c.General.DownloadDir = dir
		c.Downloads.OnDemandPar2 = true
	})
	application.log = slog.New(slog.DiscardHandler)
	jobDir := filepath.Join(dir, jobName)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin")
	return application, jobDir
}

// policies returns each recovery volume's fetch policy by subject.
func policies(t *testing.T, j *job.Job) map[string]job.FetchPolicy {
	t.Helper()
	m, p := mustManifest(t, j), j.Progress()
	out := make(map[string]job.FetchPolicy)
	for fi := range m.NumFiles() {
		if m.Files().IsPar2Recovery(fi) {
			out[m.Files().Subject(fi)] = p.FileFetchPolicy(fi)
		}
	}
	return out
}

// With no readable par2 file on disk, the first release is the one
// byte-cheapest volume, to serve as the index; a second call that still finds
// no readable set releases every remaining held volume. An index that
// describes nothing is no index (#1006).
func TestMaybeReleaseRecoveryVolumes_VolumeAsIndex(t *testing.T) {
	t.Parallel()
	for name, index := range map[string][]byte{
		"no par2 file on disk": nil,
		"a zero-filled index":  make([]byte, 4096),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testVolumeAsIndex(t, index)
		})
	}
}

func testVolumeAsIndex(t *testing.T, index []byte) {
	vols := []sizedVol{
		{"data.vol000+01.par2", 2000},
		{"data.vol001+02.par2", 1000},
		{"data.vol003+04.par2", 4000},
	}
	application := newTestApplication(t)
	dir := t.TempDir()
	application.config.With(func(c *config.Config) {
		c.General.DownloadDir = dir
		c.Downloads.OnDemandPar2 = true
	})
	application.log = slog.New(slog.DiscardHandler)
	if err := os.MkdirAll(filepath.Join(dir, "vai-name"), 0o750); err != nil {
		t.Fatal(err)
	}
	if index != nil {
		if err := os.WriteFile(filepath.Join(dir, "vai-name", "data.par2"), index, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	j, hdr := newSizedJob(t, "vai", "vai-name", types.PPRepair, []int{4065}, vols)
	if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if !application.maybeReleaseRecoveryVolumes(t.Context(), j) {
		t.Fatal("first call = false, want true: a volume is fetched to use as the index")
	}
	want := map[string]job.FetchPolicy{
		"data.vol000+01.par2": job.FetchIfNeeded,
		"data.vol001+02.par2": job.FetchAlways,
		"data.vol003+04.par2": job.FetchIfNeeded,
	}
	if got := policies(t, j); !mapsEqual(got, want) {
		t.Errorf("policies = %v, want %v: only the byte-cheapest volume is released", got, want)
	}
	if r := j.Par2ReleaseReason(); !strings.Contains(r, "use as the index") {
		t.Errorf("reason = %q, want it to say the volume is fetched to use as the index", r)
	}

	// The volume arrives but no par2 file parses from the directory.
	if err := j.MarkFileComplete(fileIdxBySubject(t, j, "data.vol001+02.par2")); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if !application.maybeReleaseRecoveryVolumes(t.Context(), j) {
		t.Fatal("second call = false, want true: the remaining volumes are released")
	}
	for name, p := range policies(t, j) {
		if p != job.FetchAlways {
			t.Errorf("%s policy = %v, want FetchAlways after the index volume did not parse", name, p)
		}
	}
	if r := j.Par2ReleaseReason(); !strings.Contains(r, "did not parse") {
		t.Errorf("reason = %q, want it to say the fetched volume did not parse", r)
	}
}

func TestMaybeReleaseRecoveryVolumes_SizedRelease(t *testing.T) {
	t.Parallel()
	arts := []int{artHead, artMid, artTail}

	// add queues a job with its interior article lost.
	add := func(t *testing.T, application *Application, id string, pp int) (*job.Job, int) {
		t.Helper()
		j, hdr := newSizedJob(t, id, id+"-name", pp, arts, sizedVols)
		if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		dataIdx := fileIdxBySubject(t, j, "data.bin")
		lo, _ := mustManifest(t, j).FileRange(dataIdx)
		if err := j.MarkArticleFailed(lo+1, job.WholeArticle); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
		return j, dataIdx
	}

	// One readable set is enough for the assessment: an unreadable set sorted
	// before it does not send the job down the no-index path (#1006), and the
	// unreadable set holds the volumes (#1000).
	t.Run("an unreadable set beside a readable one is assessed", func(t *testing.T) {
		t.Parallel()
		application, jobDir := releaseApp(t, "beside-name")
		if err := os.WriteFile(filepath.Join(jobDir, "aaa.par2"), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		j, dataIdx := add(t, application, "beside", types.PPRepair)
		application.durable = &deficitRowsStore{durabilityStore: application.durable, rows: rowsExcept(t, j, dataIdx, 1)}

		application.maybeReleaseRecoveryVolumes(t.Context(), j)
		if r := j.Par2ReleaseReason(); strings.Contains(r, "use as the index") || !strings.Contains(r, "describes no file") {
			t.Errorf("reason = %q, want the assessment's unsized release for an unreadable set, not the no-index fetch", r)
		}
	})

	// A read the cancellation cut short leaves the release unsized, which
	// would release every held volume (#1008).
	t.Run("a cancelled assessment releases nothing", func(t *testing.T) {
		t.Parallel()
		application, _ := releaseApp(t, "cancel-name")
		j, _ := add(t, application, "cancel", types.PPRepair)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		application.durable = &cancellingRowsStore{durabilityStore: application.durable, cancel: cancel}

		if application.maybeReleaseRecoveryVolumes(ctx, j) {
			t.Error("maybeReleaseRecoveryVolumes = true, want false under cancellation")
		}
		for name, p := range policies(t, j) {
			if p != job.FetchIfNeeded {
				t.Errorf("%s policy = %v, want FetchIfNeeded: a cancelled assessment releases nothing", name, p)
			}
		}
		if r := j.Par2ReleaseReason(); r != "" {
			t.Errorf("reason = %q, want none: no verdict was acted on", r)
		}
	})

	t.Run("fetches the cheapest cover and nothing once the deficit is covered", func(t *testing.T) {
		t.Parallel()
		application, _ := releaseApp(t, "sized-name")
		j, dataIdx := add(t, application, "sized", types.PPRepair)
		application.durable = &deficitRowsStore{durabilityStore: application.durable, rows: rowsExcept(t, j, dataIdx, 1)}

		if !application.maybeReleaseRecoveryVolumes(t.Context(), j) {
			t.Fatal("maybeReleaseRecoveryVolumes = false, want true: ten damaged slices need recovery blocks")
		}
		want := map[string]job.FetchPolicy{
			"data.vol000+04.par2": job.FetchAlways,
			"data.vol004+08.par2": job.FetchAlways,
			"data.vol012+16.par2": job.FetchIfNeeded,
		}
		if got := policies(t, j); !mapsEqual(got, want) {
			t.Errorf("policies = %v, want %v: only the byte-minimal cover of 10 blocks is released", got, want)
		}
		reason := j.Par2ReleaseReason()
		if reason == "" {
			t.Error("Par2ReleaseReason is empty after a release")
		}
		if strings.Contains(reason, "releasing every recovery volume") {
			t.Errorf("reason = %q: a sized release must not say it released everything", reason)
		}

		// The released volumes arrive intact (complete, nothing marks them
		// failed), so their 12 blocks cover the 10 damaged slices.
		for subject, p := range want {
			if p == job.FetchAlways {
				if err := j.MarkFileComplete(fileIdxBySubject(t, j, subject)); err != nil {
					t.Fatalf("MarkFileComplete(%s): %v", subject, err)
				}
			}
		}
		if application.maybeReleaseRecoveryVolumes(t.Context(), j) {
			t.Error("a second call = true, want false: the deficit is covered")
		}
		if got := policies(t, j); !mapsEqual(got, want) {
			t.Errorf("policies after the second call = %v, want %v: nothing more may be released", got, want)
		}
	})

	mismatchJob := func(t *testing.T, application *Application, id string) *job.Job {
		t.Helper()
		j, hdr := newSizedJob(t, id, id+"-name", types.PPRepair, []int{4065}, sizedVols)
		seedFileCRC(t, j, fileIdxBySubject(t, j, "data.bin"), 0xDEADBEEF)
		if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		return j
	}

	t.Run("a CRC mismatch is sized by the block-level verify", func(t *testing.T) {
		t.Parallel()
		application, jobDir := releaseApp(t, "mismatch-name")
		corruptPayload(t, jobDir)
		j := mismatchJob(t, application, "mismatch")
		if !application.maybeReleaseRecoveryVolumes(t.Context(), j) {
			t.Fatal("maybeReleaseRecoveryVolumes = false, want true")
		}
		want := map[string]job.FetchPolicy{
			"data.vol000+04.par2": job.FetchAlways,
			"data.vol004+08.par2": job.FetchIfNeeded,
			"data.vol012+16.par2": job.FetchIfNeeded,
		}
		if got := policies(t, j); !mapsEqual(got, want) {
			t.Errorf("policies = %v, want %v: one damaged block needs the cheapest volume only", got, want)
		}
		if reason := j.Par2ReleaseReason(); strings.Contains(reason, "releasing every recovery volume") {
			t.Errorf("reason = %q: a sized release must not say it released everything", reason)
		}
	})

	t.Run("a CRC mismatch on a file that is intact on disk releases nothing", func(t *testing.T) {
		t.Parallel()
		application, _ := releaseApp(t, "intact-name")
		j := mismatchJob(t, application, "intact")
		if application.maybeReleaseRecoveryVolumes(t.Context(), j) {
			t.Error("maybeReleaseRecoveryVolumes = true, want false: no recovery block is needed")
		}
		for name, p := range policies(t, j) {
			if p != job.FetchIfNeeded {
				t.Errorf("%s policy = %v, want FetchIfNeeded", name, p)
			}
		}
	})

	t.Run("two par2 sets on disk release every volume", func(t *testing.T) {
		t.Parallel()
		application, jobDir := releaseApp(t, "twin-name")
		corruptPayload(t, jobDir)
		// The same set under another name: one SetID and slice size, two sets.
		b, err := os.ReadFile(filepath.Join(jobDir, "data.par2"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(jobDir, "twin.par2"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		j := mismatchJob(t, application, "twin")
		if !application.maybeReleaseRecoveryVolumes(t.Context(), j) {
			t.Fatal("maybeReleaseRecoveryVolumes = false, want true")
		}
		for name, p := range policies(t, j) {
			if p != job.FetchAlways {
				t.Errorf("%s policy = %v, want FetchAlways: an unsized release frees every volume", name, p)
			}
		}
		if reason := j.Par2ReleaseReason(); !strings.Contains(reason, "releasing every recovery volume") {
			t.Errorf("reason = %q, want it to say every volume was released", reason)
		}
	})

	t.Run("no durability store releases every volume", func(t *testing.T) {
		t.Parallel()
		application, _ := releaseApp(t, "nodur-name")
		j, _ := add(t, application, "nodur", types.PPRepair)
		application.durable = nil
		if !application.maybeReleaseRecoveryVolumes(t.Context(), j) {
			t.Fatal("maybeReleaseRecoveryVolumes = false, want true")
		}
		for name, p := range policies(t, j) {
			if p != job.FetchAlways {
				t.Errorf("%s policy = %v, want FetchAlways", name, p)
			}
		}
		if reason := j.Par2ReleaseReason(); !strings.Contains(reason, "no durability store") {
			t.Errorf("reason = %q, want it to name the missing store", reason)
		}
	})

	t.Run("a job that may not repair releases nothing but records why", func(t *testing.T) {
		t.Parallel()
		application, _ := releaseApp(t, "norepair-name")
		j, dataIdx := add(t, application, "norepair", types.PPNone)
		application.durable = &deficitRowsStore{durabilityStore: application.durable, rows: rowsExcept(t, j, dataIdx, 1)}
		if application.maybeReleaseRecoveryVolumes(t.Context(), j) {
			t.Error("maybeReleaseRecoveryVolumes = true for a PP=0 job")
		}
		for name, p := range policies(t, j) {
			if p != job.FetchIfNeeded {
				t.Errorf("%s policy = %v, want FetchIfNeeded", name, p)
			}
		}
		if j.Par2ReleaseReason() == "" {
			t.Error("Par2ReleaseReason is empty, want the verdict recorded")
		}
	})
}

func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
