package assembler

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestOnArticleWritten_FiresOnlyAfterASuccessfulWrite(t *testing.T) {
	a := newHelperAssembler()
	var got []int32
	a.opts.OnArticleWritten = func(_ string, _ int, artIdx int32, _, _ int64, _ uint32) {
		got = append(got, artIdx)
	}
	f := newHelperFile(t, t.TempDir(), "w.dat", 1<<20)
	f.info.TotalParts = 3
	a.handleSuccessArticle(f, WriteRequest{JobID: "j", ArtIdx: 1, MessageID: "<1@x>", Offset: 0, Data: []byte("AAAA")})
	a.handleSuccessArticle(f, WriteRequest{JobID: "j", ArtIdx: 2, MessageID: "<2@x>", Offset: 2, Data: []byte("BBBB")}) // intersects, refused
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.EIO }
	a.handleSuccessArticle(f, WriteRequest{JobID: "j", ArtIdx: 3, MessageID: "<3@x>", Offset: 100, Data: []byte("CCCC")}) // faulted
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("OnArticleWritten fired for %v, want [1]", got)
	}
}

func TestOnArticleWritten_ReportsRangeAndCRC(t *testing.T) {
	a := newHelperAssembler()
	type rec struct {
		job    string
		file   int
		art    int32
		off, n int64
		crc    uint32
	}
	var got []rec
	a.opts.OnArticleWritten = func(j string, fi int, ai int32, off, n int64, crc uint32) {
		got = append(got, rec{j, fi, ai, off, n, crc})
	}
	f := newHelperFile(t, t.TempDir(), "r.dat", 1<<20)
	f.info.TotalParts = 1
	a.handleSuccessArticle(f, WriteRequest{JobID: "j", FileIdx: 4, ArtIdx: 7, MessageID: "<7@x>", Offset: 10, Data: []byte("ABCDE"), CRC32: 0xdeadbeef})
	want := rec{"j", 4, 7, 10, 5, 0xdeadbeef}
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %+v, want [%+v]", got, want)
	}
}

func TestFileInfoOwned_RefusesARestartLoser(t *testing.T) {
	a := newHelperAssembler()
	var rejected []int32
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) { rejected = append(rejected, artIdx) }
	f := newHelperFile(t, t.TempDir(), "s.dat", 1<<20)
	f.info.TotalParts = 1
	if err := f.w.owned.seed([]Range{{Off: 0, Len: 1000}}); err != nil {
		t.Fatal(err)
	}
	a.handleSuccessArticle(f, WriteRequest{JobID: "j", ArtIdx: 9, MessageID: "<9@x>", Offset: 500, Data: make([]byte, 1000)})
	if len(rejected) != 1 || rejected[0] != 9 {
		t.Errorf("rejected = %v, want [9]", rejected)
	}
}

// The open path turns FileInfo.Owned into a seed, dropping invalid ranges.
func TestOpenTargetFile_SeedsFromOwnedAndDropsInvalid(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)
	a := newHelperAssembler()
	path := filepath.Join(t.TempDir(), "o.dat")
	a.opts.FileInfo = func(string, int) (FileInfo, error) {
		return FileInfo{Path: path, ExpectedSize: 1 << 20, TotalParts: 1, Owned: []Range{
			{Off: -5, Len: 100},         // negative offset
			{Off: 2000, Len: 0},         // empty
			{Off: 3000, Len: -1},        // negative length
			{Off: maxInt64 - 1, Len: 5}, // overflows
			{Off: 0, Len: 1000},         // valid
		}}, nil
	}
	open := map[fileKey]*openFile{}
	key := fileKey{jobID: "j", fileIdx: 0}
	f, err := a.openTargetFile(key, WriteRequest{JobID: "j"}, open)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.w.handle.Close() })
	s := f.w.owned.s
	if len(s) != 1 || s[0].r != (Range{Off: 0, Len: 1000}) {
		t.Fatalf("seeded ranges = %+v, want exactly [{0 1000}]", s)
	}
	if _, owned := f.w.owned.ownerOf(Range{Off: 500, Len: 10}, articleID{artIdx: 3}); !owned {
		t.Error("valid range not owned")
	}
	if _, owned := f.w.owned.ownerOf(Range{Off: 2000, Len: 10}, articleID{artIdx: 3}); owned {
		t.Error("invalid range was seeded")
	}
}
