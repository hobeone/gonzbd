package job

import (
	"errors"
	"hash/crc32"
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/crc32util"
	"github.com/hobeone/gonzbd/internal/durability"
)

func verifiedTestJob(t *testing.T) *Job {
	t.Helper()
	j := New("verified", "verified", Policy{})
	m := newManifest([]JobFile{
		{Subject: "a.bin", Bytes: 400, Articles: []JobArticle{
			{ID: "<a0@x>", Bytes: 100}, {ID: "<a1@x>", Bytes: 100},
			{ID: "<a2@x>", Bytes: 100}, {ID: "<a3@x>", Bytes: 100},
		}},
		{Subject: "b.bin", Bytes: 100, Articles: []JobArticle{{ID: "<b0@x>", Bytes: 100}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j
}

// chainRows is file 0 of verifiedTestJob written whole: four abutting rows
// whose CRCs are those of data's four 100-byte slices.
func chainRows(data []byte) []durability.WrittenRow {
	rows := make([]durability.WrittenRow, 0, 4)
	for i := range 4 {
		off := int64(i * 100)
		rows = append(rows, durability.WrittenRow{
			FileIdx: 0, ArtIdx: int32(i), Offset: off, Length: 100, //nolint:gosec // G115: test index
			CRC32: crc32.ChecksumIEEE(data[off : off+100]),
		})
	}
	return rows
}

func chainData() []byte {
	data := make([]byte, 400)
	for i := range data {
		data[i] = byte(i*13 + 5)
	}
	return data
}

// TestInstallVerified_MarksDoneAndKeepsRowsInOffsetOrder pins the door's two
// effects: each row's article leaves the unfinished count, and the rows are
// kept for the whole-file CRC in offset order whatever order they arrived in.
func TestInstallVerified_MarksDoneAndKeepsRowsInOffsetOrder(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	before, err := j.CountUnfinishedArticles(0)
	if err != nil {
		t.Fatalf("CountUnfinishedArticles: %v", err)
	}

	rows := []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x2},
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1},
	}
	if dropped, err := j.InstallVerified(0, rows); err != nil || dropped != 0 {
		t.Fatalf("InstallVerified = %d, %v; want 0, nil", dropped, err)
	}

	after, err := j.CountUnfinishedArticles(0)
	if err != nil {
		t.Fatalf("CountUnfinishedArticles: %v", err)
	}
	if before-after != 2 {
		t.Errorf("unfinished articles went %d -> %d, want a drop of 2", before, after)
	}
	want := []durability.WrittenRow{rows[1], rows[0]}
	if got := j.FileRows(0); !slices.Equal(got, want) {
		t.Errorf("FileRows(0) = %+v, want %+v", got, want)
	}
	if got := j.FileRows(1); len(got) != 0 {
		t.Errorf("FileRows(1) = %+v, want none: rows are per file", got)
	}
	if p := j.Progress(); p.PendingArticles() != 3 {
		t.Errorf("PendingArticles = %d, want 3: the counters must follow the bits", p.PendingArticles())
	}
}

// TestInstallVerified_ReplacesAnArticlesEarlierRow pins that a second install
// of the same article replaces its row rather than adding one, so the CRC
// chain cannot see a duplicate.
func TestInstallVerified_ReplacesAnArticlesEarlierRow(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	first := durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0xA}
	second := durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0xB}
	for _, r := range []durability.WrittenRow{first, second} {
		if _, err := j.InstallVerified(0, []durability.WrittenRow{r}); err != nil {
			t.Fatalf("InstallVerified: %v", err)
		}
	}
	if got := j.FileRows(0); !slices.Equal(got, []durability.WrittenRow{second}) {
		t.Errorf("FileRows(0) = %+v, want only the later row", got)
	}
}

// TestInstallVerified_MergesALaterInstallWithTheResidentRows pins that a
// second install of different articles adds to the file's rows rather than
// replacing them.
func TestInstallVerified_MergesALaterInstallWithTheResidentRows(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	a := durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1}
	c := durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x3}
	for _, r := range []durability.WrittenRow{c, a} {
		if _, err := j.InstallVerified(0, []durability.WrittenRow{r}); err != nil {
			t.Fatalf("InstallVerified: %v", err)
		}
	}
	if got := j.FileRows(0); !slices.Equal(got, []durability.WrittenRow{a, c}) {
		t.Errorf("FileRows(0) = %+v, want both rows in offset order", got)
	}
}

// TestInstallVerified_ARowItCannotPlaceCostsOnlyItself pins open design item
// 3: a row naming another file, an article outside the file, or an impossible
// range is dropped and counted, and the valid rows beside it are installed
// (Standing Design Rule 3).
func TestInstallVerified_ARowItCannotPlaceCostsOnlyItself(t *testing.T) {
	t.Parallel()
	good := durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}
	for name, bad := range map[string]durability.WrittenRow{
		// Article 1 is inside file 0's range, so only the file check refuses it.
		"another file's row":          {FileIdx: 1, ArtIdx: 1, Offset: 100, Length: 100},
		"an article outside the file": {FileIdx: 0, ArtIdx: 4, Offset: 400, Length: 100},
		"a negative article":          {FileIdx: 0, ArtIdx: -1, Offset: 0, Length: 100},
		"a negative offset":           {FileIdx: 0, ArtIdx: 1, Offset: -1, Length: 100},
		"a negative length":           {FileIdx: 0, ArtIdx: 1, Offset: 100, Length: -1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			j := verifiedTestJob(t)
			dropped, err := j.InstallVerified(0, []durability.WrittenRow{good, bad})
			if err != nil || dropped != 1 {
				t.Fatalf("InstallVerified = %d, %v; want 1, nil", dropped, err)
			}
			if n, _ := j.CountUnfinishedArticles(0); n != 3 {
				t.Errorf("unfinished = %d, want 3: the valid row is installed", n)
			}
			if got := j.FileRows(0); !slices.Equal(got, []durability.WrittenRow{good}) {
				t.Errorf("FileRows(0) = %+v, want only the valid row", got)
			}
		})
	}
	j := verifiedTestJob(t)
	if _, err := j.InstallVerified(2, nil); err == nil {
		t.Error("InstallVerified accepted an out-of-range file index")
	}
}

// TestInstallVerified_NeedsTheManifest pins the residency gate: installing
// maintains counters, which need the manifest.
func TestInstallVerified_NeedsTheManifest(t *testing.T) {
	t.Parallel()
	j := New("cold", "cold", Policy{})
	_, err := j.InstallVerified(0, []durability.WrittenRow{{FileIdx: 0}})
	if !errors.Is(err, ErrNotResident) {
		t.Errorf("InstallVerified on a non-resident job = %v, want ErrNotResident", err)
	}
	if got := j.FileRows(0); got != nil {
		t.Errorf("FileRows on a job with no progress = %+v, want nil", got)
	}
}

// TestFileRows_ReturnsACopy pins that a caller cannot edit the resident rows
// through the returned slice, and that a Progress() clone's view of the rows
// does not move when the job's rows do.
func TestFileRows_ReturnsACopy(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	if _, err := j.InstallVerified(0, []durability.WrittenRow{{FileIdx: 0, ArtIdx: 0, Length: 100, CRC32: 7}}); err != nil {
		t.Fatalf("InstallVerified: %v", err)
	}
	got := j.FileRows(0)
	got[0].CRC32 = 99
	if again := j.FileRows(0); again[0].CRC32 != 7 {
		t.Errorf("resident row CRC = %d after editing the returned copy, want 7", again[0].CRC32)
	}
	clone := j.Progress()
	if _, err := j.InstallVerified(0, []durability.WrittenRow{{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100}}); err != nil {
		t.Fatalf("InstallVerified: %v", err)
	}
	if err := j.MarkArticleWritten(durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Length: 100, CRC32: 8}); err != nil {
		t.Fatalf("MarkArticleWritten: %v", err)
	}
	if n := len(clone.written[0]); n != 1 || clone.written[0][0].CRC32 != 7 {
		t.Errorf("a Progress() clone sees %+v after later writes, want its own one row with CRC 7", clone.written[0])
	}
}

// TestInstallVerified_NeitherKeepsNorReordersTheCallersSlice pins the first
// install into a file: the resident rows are a sorted copy, so the caller can
// reuse or edit its slice without reaching the job.
func TestInstallVerified_NeitherKeepsNorReordersTheCallersSlice(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	rows := []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x2},
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1},
	}
	if _, err := j.InstallVerified(0, rows); err != nil {
		t.Fatalf("InstallVerified: %v", err)
	}
	if rows[0].ArtIdx != 2 || rows[1].ArtIdx != 0 {
		t.Errorf("the caller's slice was reordered: %+v", rows)
	}
	rows[1].CRC32 = 99
	if got := j.FileRows(0); got[0].CRC32 != 0x1 {
		t.Errorf("resident row CRC = %d after editing the caller's slice, want 1: the job kept the caller's slice", got[0].CRC32)
	}
}

// TestInstallRows_KeepsACopyOfTheFirstInstall pins installRows' own contract on
// a file with no resident rows: it stores a copy, so an edit to the caller's
// slice does not reach the job. InstallVerified and InstallFileVerification pass
// it placeRows' fresh slice, so neither door can observe this; the test calls
// installRows directly.
func TestInstallRows_KeepsACopyOfTheFirstInstall(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	rows := []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x2},
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1},
	}
	j.contentMu.Lock()
	installRows(j.manifest, j.progress, 0, rows)
	j.contentMu.Unlock()
	rows[1].CRC32 = 99
	if got := j.FileRows(0); len(got) != 2 || got[0].CRC32 != 0x1 {
		t.Errorf("FileRows(0) = %+v after editing the caller's slice, want article 0's CRC 1: installRows kept the caller's slice", got)
	}
}

// TestMarkArticleWritten_ReplacesARowAResetLeftBehind pins that a write
// replaces its article's resident row rather than adding one, including when
// the article's Done bit was cleared under the row: a failed article's late
// write stores a row, and ResetForRetry clears Done and Failed but keeps it.
func TestMarkArticleWritten_ReplacesARowAResetLeftBehind(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	if err := j.MarkArticleFailed(0); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if err := j.MarkArticleWritten(durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1}); err != nil {
		t.Fatalf("MarkArticleWritten: %v", err)
	}
	j.ResetForRetry()
	second := durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 2}
	if err := j.MarkArticleWritten(second); err != nil {
		t.Fatalf("MarkArticleWritten: %v", err)
	}
	if got := j.FileRows(0); !slices.Equal(got, []durability.WrittenRow{second}) {
		t.Errorf("FileRows(0) = %+v, want only the second write's row", got)
	}
}

// TestMarkArticleWritten_ReplacesWithoutReachingAClone pins upsertRows'
// copy-on-write: a clone taken before a replacement shares the stored slice,
// so the replacement must land in a new one and leave the clone's row as it
// was.
func TestMarkArticleWritten_ReplacesWithoutReachingAClone(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	if err := j.MarkArticleWritten(durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Length: 100, CRC32: 7}); err != nil {
		t.Fatalf("MarkArticleWritten: %v", err)
	}
	clone := j.Progress()
	if err := j.MarkArticleWritten(durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Length: 100, CRC32: 8}); err != nil {
		t.Fatalf("MarkArticleWritten: %v", err)
	}
	if rows := clone.written[0]; len(rows) != 1 || rows[0].CRC32 != 7 {
		t.Errorf("a clone taken before the replacement sees %+v, want its one row with CRC 7", rows)
	}
}

// TestJobFileState pins the record's view of one file: each field read from
// the file's progress, and no state for a file the job does not have.
func TestJobFileState(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	if err := j.SetFileFilename(1, "b.bin"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}
	if err := j.MarkFileComplete(1); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	want := durability.FileState{FileIdx: 1, Complete: true, Filename: "b.bin", FetchPolicy: uint8(FetchIfNeeded)}
	if got, ok := j.FileState(1); !ok || got != want {
		t.Errorf("FileState(1) = %+v, %v; want %+v, true", got, ok, want)
	}
	for _, fi := range []int{-1, 2} {
		if _, ok := j.FileState(fi); ok {
			t.Errorf("FileState(%d) reported a file the job does not have", fi)
		}
	}
}

// TestManifestArticleInFile pins the one range check a written row passes: the
// file must exist and the article must sit inside that file's range.
func TestManifestArticleInFile(t *testing.T) {
	t.Parallel()
	m := verifiedTestJob(t).manifest // file 0 is articles [0,4), file 1 is [4,5)
	for _, tc := range []struct {
		file int
		art  int32
		want bool
	}{
		{0, 0, true}, {0, 3, true}, {1, 4, true},
		{0, 4, false}, {1, 3, false}, {0, -1, false},
		{-1, 0, false}, {2, 4, false},
	} {
		if got := m.ArticleInFile(tc.file, tc.art); got != tc.want {
			t.Errorf("ArticleInFile(%d, %d) = %v, want %v", tc.file, tc.art, got, tc.want)
		}
	}
}

// TestMarkArticleWritten_AcceptsAZeroLengthRow pins that the range check is
// all MarkArticleWritten asks: the assembler reports a zero-length article
// with n == 0, and it must resolve, or its file is never fully resolved.
func TestMarkArticleWritten_AcceptsAZeroLengthRow(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	if err := j.MarkArticleWritten(durability.WrittenRow{FileIdx: 1, ArtIdx: 4, Offset: 0, Length: 0}); err != nil {
		t.Fatalf("MarkArticleWritten of a zero-length article: %v", err)
	}
	p := j.Progress()
	if !p.ArticleDone(4) || p.FilePending(1) != 0 {
		t.Errorf("zero-length article Done = %v, file 1 pending = %d; want Done and nothing pending", p.ArticleDone(4), p.FilePending(1))
	}
}

// TestMarkArticleWritten_RejectsAnInvalidShape: the live door judges a row by
// WrittenRow.HasValidShape, as a restart does, so a row of an article in range
// but with a negative offset is refused and its article is not Done.
func TestMarkArticleWritten_RejectsAnInvalidShape(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	if err := j.MarkArticleWritten(durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Offset: -1, Length: 100}); err == nil {
		t.Fatal("MarkArticleWritten accepted a row with a negative offset")
	}
	if j.Progress().ArticleDone(1) {
		t.Error("an article whose row was refused is Done")
	}
	if got := j.FileRows(0); len(got) != 0 {
		t.Errorf("FileRows(0) = %+v, want none: the refused row is resident", got)
	}
}

// TestMarkArticleWritten_MarksDoneAndKeepsTheRow pins the in-process door: the
// article is Done, its row is resident for the CRC, and a second write of the
// same article replaces its row.
func TestMarkArticleWritten_MarksDoneAndKeepsTheRow(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	first := durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 1}
	second := durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 2}
	for _, r := range []durability.WrittenRow{first, second} {
		if err := j.MarkArticleWritten(r); err != nil {
			t.Fatalf("MarkArticleWritten: %v", err)
		}
	}
	if !j.Progress().ArticleDone(1) {
		t.Error("a written article is not Done")
	}
	if got := j.FileRows(0); !slices.Equal(got, []durability.WrittenRow{second}) {
		t.Errorf("FileRows(0) = %+v, want only the later row", got)
	}
	if err := j.MarkArticleWritten(durability.WrittenRow{FileIdx: 0, ArtIdx: 4}); err == nil {
		t.Error("MarkArticleWritten accepted another file's article")
	}
	if err := New("cold", "cold", Policy{}).MarkArticleWritten(first); !errors.Is(err, ErrNotResident) {
		t.Errorf("MarkArticleWritten on a non-resident job = %v, want ErrNotResident", err)
	}
}

// TestSettleFileCRC_DerivesStoresAndReleases pins the in-process completion:
// the CRC comes from the resident rows, is stored on the file, and the rows
// are released — they are not resident past completion.
func TestSettleFileCRC_DerivesStoresAndReleases(t *testing.T) {
	t.Parallel()
	data := chainData()
	j := verifiedTestJob(t)
	for _, r := range slices.Backward(chainRows(data)) {
		if err := j.MarkArticleWritten(r); err != nil {
			t.Fatalf("MarkArticleWritten: %v", err)
		}
	}
	crc, ok, err := j.SettleFileCRC(0)
	if err != nil || !ok || crc != crc32.ChecksumIEEE(data) {
		t.Fatalf("SettleFileCRC = %08x, %v, %v; want %08x, true, nil", crc, ok, err, crc32.ChecksumIEEE(data))
	}
	if got := j.Progress().FileAssembledCRC32(0); got != crc {
		t.Errorf("stored CRC = %08x, want %08x", got, crc)
	}
	if got := j.FileRows(0); got != nil {
		t.Errorf("FileRows(0) = %+v after the CRC settled, want none", got)
	}
}

// TestInstallFileVerification_CompleteFileFailsTheRestAndSettles pins open design item 1: a
// complete=1 file's failed set is the complement of its rows, the file is
// Complete, and a file with a failed article has no CRC.
func TestInstallFileVerification_CompleteFileFailsTheRestAndSettles(t *testing.T) {
	t.Parallel()
	data := chainData()
	rows := chainRows(data)

	whole := verifiedTestJob(t)
	if dropped, err := whole.InstallFileVerification(FileVerification{FileIdx: 0, Complete: true, Rows: rows}); err != nil || dropped != 0 {
		t.Fatalf("InstallFileVerification = %d, %v", dropped, err)
	}
	p := whole.Progress()
	if !p.FileComplete(0) || p.FileAssembledCRC32(0) != crc32.ChecksumIEEE(data) {
		t.Errorf("a whole complete file: complete=%v crc=%08x; want true, %08x",
			p.FileComplete(0), p.FileAssembledCRC32(0), crc32.ChecksumIEEE(data))
	}
	for i := range 4 {
		if p.ArticleFailed(i) {
			t.Errorf("article %d of a whole complete file is failed", i)
		}
	}

	holed := verifiedTestJob(t)
	bad := durability.WrittenRow{FileIdx: 0, ArtIdx: 4, Offset: 0, Length: 100}
	dropped, err := holed.InstallFileVerification(FileVerification{FileIdx: 0, Complete: true, Rows: []durability.WrittenRow{rows[0], rows[2], bad}})
	if err != nil || dropped != 1 {
		t.Fatalf("InstallFileVerification = %d, %v; want 1, nil", dropped, err)
	}
	p = holed.Progress()
	for art, wantFailed := range []bool{false, true, false, true} {
		if !p.ArticleDone(art) || p.ArticleFailed(art) != wantFailed {
			t.Errorf("article %d: done=%v failed=%v, want done and failed=%v", art, p.ArticleDone(art), p.ArticleFailed(art), wantFailed)
		}
	}
	if p.ArticleDone(4) {
		t.Error("another file's article is Done from a corrupt row")
	}
	if p.FileAssembledCRC32(0) != 0 {
		t.Error("a file with failed articles has a CRC")
	}
	if holed.FileRows(0) != nil {
		t.Error("a complete file's rows are still resident")
	}
}

// TestUntrustFile_ReturnsTheFileToOutstanding pins the in-memory half of an
// untrust: Done bits, Complete, the CRC and the resident rows all go, and a
// failed article stays failed.
func TestUntrustFile_ReturnsTheFileToOutstanding(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	for _, r := range chainRows(chainData())[:3] {
		if err := j.MarkArticleWritten(r); err != nil {
			t.Fatalf("MarkArticleWritten: %v", err)
		}
	}
	if err := j.MarkArticleFailed(3); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if err := j.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if err := j.UntrustFile(0); err != nil {
		t.Fatalf("UntrustFile: %v", err)
	}
	p := j.Progress()
	for art := range 3 {
		if p.ArticleDone(art) {
			t.Errorf("article %d is Done after the untrust", art)
		}
	}
	if !p.ArticleFailed(3) {
		t.Error("a failed article was un-failed by the untrust")
	}
	if p.FileComplete(0) || j.FileRows(0) != nil {
		t.Error("the untrusted file is still Complete or still has rows")
	}
	if n, _ := j.CountUnfinishedArticles(0); n != 3 {
		t.Errorf("unfinished = %d after the untrust, want 3", n)
	}
	if p.PendingArticles() != 4 {
		t.Errorf("PendingArticles = %d, want 4: the counters must follow the bits", p.PendingArticles())
	}
}

// TestFileCRCFromRows pins the derivation's single-chain predicate: a CRC
// only from a gapless chain of every article of the file, starting at 0, with
// none failed.
func TestFileCRCFromRows(t *testing.T) {
	t.Parallel()
	data := chainData()
	// File 1 of a job whose file 0 has 3 articles, so lo is not 0.
	const lo, hi = 3, 7
	chain := func() []durability.WrittenRow {
		rows := chainRows(data)
		for i := range rows {
			rows[i].FileIdx, rows[i].ArtIdx = 1, int32(lo+i) //nolint:gosec // G115: test index
		}
		return rows
	}

	got, ok := fileCRCFromRows(chain(), false, lo, hi)
	if !ok || got != crc32.ChecksumIEEE(data) {
		t.Errorf("gapless chain = %08x, %v; want %08x, true", got, ok, crc32.ChecksumIEEE(data))
	}
	if got, ok := fileCRCFromRows(chainRows(data), false, 0, 4); !ok || got != crc32.ChecksumIEEE(data) {
		t.Errorf("gapless chain at lo=0 = %08x, %v; want %08x, true", got, ok, crc32.ChecksumIEEE(data))
	}

	for name, mutate := range map[string]func([]durability.WrittenRow) []durability.WrittenRow{
		"a gap": func(r []durability.WrittenRow) []durability.WrittenRow {
			r[2].Offset++
			r[3].Offset++
			return r
		},
		"a straddle": func(r []durability.WrittenRow) []durability.WrittenRow {
			r[1].Offset = 50
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

// TestPlaceRows_KeepsOnlyRowsOfTheFile pins the split InstallVerified and
// InstallFileVerification share: a row is kept only when it names the file, lies in
// its range and has a valid shape (a zero-length row is valid); every other row
// is counted.
func TestPlaceRows_KeepsOnlyRowsOfTheFile(t *testing.T) {
	t.Parallel()
	m := verifiedTestJob(t).manifest
	good := durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100}
	rows := []durability.WrittenRow{
		good,
		{FileIdx: 1, ArtIdx: 1, Offset: 100, Length: 100}, // names another file
		{FileIdx: 0, ArtIdx: 4, Offset: 400, Length: 100}, // file 1's article
		{FileIdx: 0, ArtIdx: 2, Offset: -1, Length: 100},  // negative offset
		{FileIdx: 0, ArtIdx: 3, Offset: 300, Length: -1},  // negative length
	}
	kept, dropped, err := placeRows(m, 0, rows)
	if err != nil {
		t.Fatalf("placeRows: %v", err)
	}
	if !slices.Equal(kept, []durability.WrittenRow{good}) || dropped != 4 {
		t.Errorf("placeRows = %+v, %d dropped; want only the good row and 4 dropped", kept, dropped)
	}
	empty := durability.WrittenRow{FileIdx: 0, ArtIdx: 3, Offset: 300, Length: 0}
	if kept, dropped, _ := placeRows(m, 0, []durability.WrittenRow{empty}); !slices.Equal(kept, []durability.WrittenRow{empty}) || dropped != 0 {
		t.Errorf("placeRows of a zero-length row = %+v, %d dropped; want it kept: the live door accepts it", kept, dropped)
	}
	if _, _, err := placeRows(m, 2, rows); err == nil {
		t.Error("placeRows accepted a file index past the manifest")
	}
}

// TestSettleFileCRC_RefusesWhatItCannotSettle pins SettleFileCRC's two
// refusals: a job without its manifest, and a file index past it.
func TestSettleFileCRC_RefusesWhatItCannotSettle(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	if _, _, err := j.SettleFileCRC(2); err == nil {
		t.Error("SettleFileCRC accepted a file index past the manifest")
	}
	j.Evict()
	if _, _, err := j.SettleFileCRC(0); !errors.Is(err, ErrNotResident) {
		t.Errorf("SettleFileCRC on an evicted job = %v, want ErrNotResident", err)
	}
}
