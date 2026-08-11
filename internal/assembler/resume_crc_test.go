package assembler

import (
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// The tests in this file cover #349: finalizeFile combined whatever per-article
// CRCs the current run happened to record and reported the result as the
// whole-file CRC, without checking that those parts tile the file.
//
// crc32util.Combine reconstructs CRC(A||B) from the two parts' CRCs and B's
// length alone. It is given no offsets, so the fold yields the CRC of the parts
// concatenated — which is the file's CRC only when they tile [0, extent)
// exactly. On resume the previous run's articles are not re-dispatched, so they
// contribute no part and the fold silently describes a subrange.
//
// Every article below carries a real non-zero CRC. That is load-bearing: the
// resume tests in resume_extent_test.go leave WriteRequest.CRC unset, which
// trips recordArticleCRC's zero-CRC branch and invalidates the file CRC through
// an entirely different path. A test inheriting that omission would report 0
// both before and after the fix — vacuously green, and indistinguishable from a
// real pass.

// seedPriorRun writes an earlier run's bytes to path and returns them.
func seedPriorRun(t *testing.T, path string) []byte {
	t.Helper()
	prior := []byte("0123456789ABCDEFGHIJ")
	if err := os.WriteFile(path, prior, 0o644); err != nil {
		t.Fatalf("seed prior run's file: %v", err)
	}
	return prior
}

// runToCompletion drives one file to completion and returns the CRC reported to
// OnFileComplete.
func runToCompletion(t *testing.T, opts Options, reqs ...WriteRequest) uint32 {
	t.Helper()
	var gotCRC uint32
	done := make(chan struct{}, 1)
	opts.OnFileComplete = func(_ string, _ int, fileCRC uint32) {
		gotCRC = fileCRC
		done <- struct{}{}
	}
	a := startAssembler(t, opts)
	for _, req := range reqs {
		if err := a.WriteArticle(t.Context(), req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}
	<-done
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return gotCRC
}

// TestResumedFileWithNewPrefixReportsNoCRC covers the shape where the resumed
// run's one outstanding article sits at the front of the file. The recorded
// parts start at offset 0 but stop far short of the extent, so the fold used to
// return the CRC of that 4-byte prefix as if it described all 20 bytes.
func TestResumedFileWithNewPrefixReportsNoCRC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job1_0.dat")
	prior := seedPriorRun(t, path)

	data := []byte("ZZZZ")
	opts := makeOpts(dir, map[string]FileInfo{
		"job1:0": {
			Path:              path,
			TotalParts:        1, // only the still-unfinished article
			InitialMaxWritten: int64(len(prior)),
		},
	})

	got := runToCompletion(t, opts, WriteRequest{
		JobID: "job1", FileIdx: 0, Offset: 0,
		Data: data, CRC: crc32.ChecksumIEEE(data),
	})

	if got != 0 {
		t.Errorf("reported file CRC = %#08x, want 0.\n"+
			"The run recorded one 4-byte part but the file is %d bytes, so no "+
			"whole-file CRC is knowable and it must be reported as unavailable "+
			"rather than as the CRC of the part that happened to be written (#349).\n"+
			"CRC of the new article alone = %#08x",
			got, len(prior), crc32.ChecksumIEEE(data))
	}
}

// TestResumedFileWithNewSuffixReportsNoCRC covers the other resume shape: the
// outstanding article sits at the end, so the recorded parts do not begin at
// offset 0. This exercises a different clause of the predicate than the prefix
// case — offset mismatch rather than a short span.
func TestResumedFileWithNewSuffixReportsNoCRC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job1_0.dat")
	prior := seedPriorRun(t, path)

	data := []byte("ZZZZ")
	offset := int64(len(prior) - len(data))
	opts := makeOpts(dir, map[string]FileInfo{
		"job1:0": {
			Path:              path,
			TotalParts:        1,
			InitialMaxWritten: int64(len(prior)),
		},
	})

	got := runToCompletion(t, opts, WriteRequest{
		JobID: "job1", FileIdx: 0, Offset: offset,
		Data: data, CRC: crc32.ChecksumIEEE(data),
	})

	if got != 0 {
		t.Errorf("reported file CRC = %#08x, want 0.\n"+
			"The only recorded part starts at offset %d, so the fold describes a "+
			"suffix rather than the file (#349).", got, offset)
	}
}

// TestCompleteFileReportsWholeFileCRC pins the green path. Without it the guard
// added for #349 would be satisfiable by always reporting 0, and no existing
// test asserts a non-zero combined CRC — assembler_test.go's finalizeFile
// subtest discards the value and writeoffset_test.go asserts it is zero.
//
// The expected value comes from crc32.ChecksumIEEE over the bytes actually on
// disk, so the assertion rests on an independent oracle rather than on
// crc32util.Combine agreeing with itself.
func TestCompleteFileReportsWholeFileCRC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job1_0.dat")

	first, second := []byte("abcd"), []byte("efgh")
	opts := makeOpts(dir, map[string]FileInfo{
		"job1:0": {Path: path, TotalParts: 2},
	})

	got := runToCompletion(t, opts,
		WriteRequest{
			JobID: "job1", FileIdx: 0, Offset: 0,
			Data: first, CRC: crc32.ChecksumIEEE(first),
		},
		WriteRequest{
			JobID: "job1", FileIdx: 0, Offset: int64(len(first)),
			Data: second, CRC: crc32.ChecksumIEEE(second),
		},
	)

	want := crc32.ChecksumIEEE(readFile(t, path))
	if want == 0 {
		t.Fatal("oracle CRC is 0; the fixture cannot distinguish a reported CRC " +
			"from the unavailable sentinel")
	}
	if got != want {
		t.Errorf("reported file CRC = %#08x, want %#08x (crc32.ChecksumIEEE of "+
			"the bytes on disk)", got, want)
	}
}

// finalizeOnce runs finalizeFile against a hand-built openFile and returns the
// CRC reported to OnFileComplete, so the tests below can drive the individual
// truncate branches directly.
func finalizeOnce(t *testing.T, f *openFile) uint32 {
	t.Helper()
	var gotCRC uint32
	var fired int
	a := New(Options{
		FileInfo: func(string, int) (FileInfo, error) { return FileInfo{Path: "test"}, nil },
		OnFileComplete: func(_ string, _ int, fileCRC uint32) {
			gotCRC = fileCRC
			fired++
		},
	}, nil)
	a.pendingDone = make(map[string][]string)
	a.pendingFailed = make(map[string][]string)

	key := fileKey{jobID: "job1", fileIdx: 0}
	a.finalizeFile(f, key, WriteRequest{JobID: "job1", FileIdx: 0},
		map[fileKey]*openFile{key: f}, make(map[fileKey]struct{}), newWriteCache(0))

	if fired != 1 {
		t.Fatalf("OnFileComplete fired %d times, want 1", fired)
	}
	return gotCRC
}

// tilingParts returns parts that tile [0, len(body)) exactly, so the coverage
// check passes and whatever the test asserts is about the truncate branch
// rather than about coverage.
func tilingParts(body []byte) []crcPart {
	half := int64(len(body) / 2)
	return []crcPart{
		{offset: 0, crc: crc32.ChecksumIEEE(body[:half]), len: half},
		{offset: half, crc: crc32.ChecksumIEEE(body[half:]), len: int64(len(body)) - half},
	}
}

// TestUntrimmedFileReportsNoCRC covers each finalizeFile branch that leaves the
// file longer than maxWritten. In all three the recorded parts tile
// [0, maxWritten) exactly, so the coverage check passes — but the pre-allocated
// trailing zeros are still on disk, so a CRC over that range describes bytes
// the file does not have and par2 would read it as a mismatch. Same false
// corruption claim as #349, one branch over.
//
// The three branches are provoked separately because they fail for different
// reasons and a single fixture cannot reach them all.
func TestUntrimmedFileReportsNoCRC(t *testing.T) {
	body := []byte("hello world")

	t.Run("stat fails", func(t *testing.T) {
		// A closed handle makes Stat fail, taking the statErr branch.
		fh := writeThenReopen(t, body, os.O_RDWR)
		if err := fh.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		got := finalizeOnce(t, &openFile{
			handle: fh, maxWritten: int64(len(body)),
			crcValid: true, crcParts: tilingParts(body),
		})
		if got != 0 {
			t.Errorf("reported file CRC = %#08x, want 0; the file could not be "+
				"stat'd, so it was left untrimmed", got)
		}
	})

	t.Run("truncate target exceeds the file on disk", func(t *testing.T) {
		// maxWritten above the real size takes the overshoot branch, which
		// deliberately skips the truncate and leaves the file short.
		fh := writeThenReopen(t, body, os.O_RDWR)
		over := int64(len(body)) + 8
		parts := tilingParts(body)
		parts = append(parts, crcPart{offset: int64(len(body)), crc: 0, len: 8})
		got := finalizeOnce(t, &openFile{
			handle: fh, maxWritten: over,
			crcValid: true, crcParts: parts,
		})
		if got != 0 {
			t.Errorf("reported file CRC = %#08x, want 0; maxWritten (%d) exceeds "+
				"the file on disk (%d), so it was left untrimmed",
				got, over, len(body))
		}
	})

	t.Run("truncate fails", func(t *testing.T) {
		// A read-only handle lets Stat succeed and reach the default branch,
		// where Truncate then fails — distinct from the stat-failure case.
		fh := writeThenReopen(t, body, os.O_RDONLY)
		// The parts must tile [0, maxWritten) exactly, or the coverage check
		// would reject them first and the assertion would pass without ever
		// reaching the truncate branch this subtest exists to cover.
		trimmed := int64(len(body)) - 4
		got := finalizeOnce(t, &openFile{
			handle: fh, maxWritten: trimmed,
			crcValid: true,
			crcParts: []crcPart{
				{offset: 0, crc: crc32.ChecksumIEEE(body[:trimmed]), len: trimmed},
			},
		})
		if got != 0 {
			t.Errorf("reported file CRC = %#08x, want 0; the truncate failed, so "+
				"the file kept bytes the recorded parts do not describe", got)
		}
	})
}

// writeThenReopen writes body to a fresh temp file and reopens it with the
// given flags, so a test can hold a handle whose permissions make a specific
// finalizeFile syscall fail.
func writeThenReopen(t *testing.T, body []byte, flag int) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "job1_0.dat")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	fh, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		t.Fatalf("reopen %v: %v", flag, err)
	}
	t.Cleanup(func() { _ = fh.Close() })
	return fh
}

// TestFailedCachedWriteInvalidatesFileCRC covers the drained-write failure path.
// The inline write path clears crcValid on a failed WriteAt; a drained one must
// too, or a later article can raise maxWritten past the failed range and leave
// the parts tiling it, yielding a confident CRC over content the disk lacks.
func TestFailedCachedWriteInvalidatesFileCRC(t *testing.T) {
	dir := t.TempDir()
	fh, err := os.Create(filepath.Join(dir, "job1_0.dat"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A closed handle makes every WriteAt fail.
	if err := fh.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	a := New(Options{
		FileInfo: func(string, int) (FileInfo, error) { return FileInfo{Path: "test"}, nil },
	}, nil)
	f := &openFile{handle: fh, crcValid: true}
	a.writeCachedArticles(f, []bufferedArticle{
		{offset: 0, data: []byte("abcd")},
	}, "test drain")

	if f.crcValid {
		t.Error("crcValid is still true after a failed cached write; the file " +
			"is missing those bytes, so no whole-file CRC is reportable")
	}
}

// TestCombineWholeFileCRC exercises the tiling predicate directly.
//
// Every expected value is computed with crc32.ChecksumIEEE over the same bytes
// the parts describe, so the table pins the fold against an independent oracle
// rather than against crc32util.Combine agreeing with itself.
func TestCombineWholeFileCRC(t *testing.T) {
	file := []byte("the quick brown fox jumps over the lazy dog")
	// part builds a crcPart describing file[off:off+n].
	part := func(off, n int64) crcPart {
		return crcPart{offset: off, crc: crc32.ChecksumIEEE(file[off : off+n]), len: n}
	}
	total := int64(len(file))

	tests := []struct {
		name  string
		parts []crcPart
		total int64
		want  uint32
		ok    bool
	}{
		{
			name:  "contiguous zero-based full span",
			parts: []crcPart{part(0, 10), part(10, 20), part(30, total-30)},
			total: total,
			want:  crc32.ChecksumIEEE(file),
			ok:    true,
		},
		{
			name:  "single part spanning the whole file",
			parts: []crcPart{part(0, total)},
			total: total,
			want:  crc32.ChecksumIEEE(file),
			ok:    true,
		},
		{
			name: "zero-length part at a contiguous boundary",
			// A zero-length part must carry crc 0: Combine returns crc1
			// unchanged when len2 <= 0, so a non-zero CRC here would be
			// silently dropped and the case would pin the drop rather than
			// the boundary behaviour.
			parts: []crcPart{part(0, 10), {offset: 10, crc: 0, len: 0}, part(10, total-10)},
			total: total,
			want:  crc32.ChecksumIEEE(file),
			ok:    true,
		},
		{
			name:  "suffix only, the resume case",
			parts: []crcPart{part(10, total-10)},
			total: total,
			ok:    false,
		},
		{
			name:  "interior hole",
			parts: []crcPart{part(0, 10), part(20, total-20)},
			total: total,
			ok:    false,
		},
		{
			name:  "tiles from zero but stops short of the extent",
			parts: []crcPart{part(0, 10), part(10, 10)},
			total: total,
			ok:    false,
		},
		{
			name:  "overlapping parts",
			parts: []crcPart{part(0, 20), part(10, total-10)},
			total: total,
			ok:    false,
		},
		{
			name:  "parts extend past the extent",
			parts: []crcPart{part(0, 10), part(10, 10)},
			total: 15,
			ok:    false,
		},
		{
			name:  "empty parts tile a zero-length file",
			parts: nil,
			total: 0,
			want:  0,
			ok:    true,
		},
		{
			name:  "empty parts do not tile a non-empty file",
			parts: nil,
			total: total,
			ok:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := combineWholeFileCRC(tc.parts, tc.total)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				if got != 0 {
					t.Errorf("crc = %#08x on a rejected tiling, want 0", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("crc = %#08x, want %#08x (crc32.ChecksumIEEE of the "+
					"described bytes)", got, tc.want)
			}
		})
	}
}

// TestPartialCoverageCheckComposesWithCrcValid pins that the tiling check is an
// additional condition rather than a replacement for crcValid. Parts that tile
// the extent perfectly must still report no CRC when this run saw a failure,
// because the bytes on disk are not the ones the parts describe.
func TestPartialCoverageCheckComposesWithCrcValid(t *testing.T) {
	body := []byte("hello world")
	gotCRC := finalizeOnce(t, &openFile{
		handle:     writeThenReopen(t, body, os.O_RDWR),
		maxWritten: int64(len(body)),
		crcValid:   false, // a failed article earlier in this run
		crcParts:   tilingParts(body),
	})

	if gotCRC != 0 {
		t.Errorf("reported file CRC = %#08x, want 0. The parts tile the extent, "+
			"but crcValid is false, so the coverage check must not override it",
			gotCRC)
	}
}
