// Package jobtest holds helpers that put a job into a state for tests in other
// packages.
package jobtest

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// MarkArticleWritten marks article artIdx Done on j the way the recorder does,
// through job.Job.MarkArticleWritten, with a row that places the article at the
// sum of the sizes of the articles before it in its file. It fails the test if
// j has no resident manifest or the index is out of range.
func MarkArticleWritten(t testing.TB, j *job.Job, artIdx int) {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("MarkArticleWritten: manifest of %s: %v", j.ID(), err)
	}
	if artIdx < 0 || artIdx >= m.NumArticles() {
		t.Fatalf("MarkArticleWritten: article %d out of range (%d articles)", artIdx, m.NumArticles())
	}
	var off int64
	fileIdx := 0
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		if artIdx < lo || artIdx >= hi {
			continue
		}
		fileIdx = fi
		for i := lo; i < artIdx; i++ {
			off += int64(m.ArticleBytes(i))
		}
		break
	}
	if err := j.MarkArticleWritten(durability.WrittenRow{
		FileIdx: fileIdx,
		ArtIdx:  int32(artIdx), //nolint:gosec // G115: bounded by NumArticles above
		Offset:  off,
		Length:  int64(m.ArticleBytes(artIdx)),
	}); err != nil {
		t.Fatalf("MarkArticleWritten(%d): %v", artIdx, err)
	}
}

// SeedFileCRC gives file fileIdx the assembled CRC crc through the door
// production uses, Job.SettleFileCRC, by presenting the rows that would earn
// it: the file's first article carries the whole file at offset 0 with crc, and
// every later article is a zero-length row at the file's end, which
// crc32util.Combine folds in without changing the value. Every article of the
// file ends up Done. It fails the test if the rows do not settle to crc.
func SeedFileCRC(t testing.TB, j *job.Job, fileIdx int, crc uint32) {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("SeedFileCRC: manifest of %s: %v", j.ID(), err)
	}
	lo, hi := m.FileRange(fileIdx)
	size := m.FileBytes(fileIdx)
	for i := lo; i < hi; i++ {
		row := durability.WrittenRow{FileIdx: fileIdx, ArtIdx: int32(i), Offset: size} //nolint:gosec // G115: bounded by NumArticles
		if i == lo {
			row.Offset, row.Length, row.CRC32 = 0, size, crc
		}
		if err := j.MarkArticleWritten(row); err != nil {
			t.Fatalf("SeedFileCRC: MarkArticleWritten(%d): %v", i, err)
		}
	}
	got, ok, err := j.SettleFileCRC(fileIdx)
	if err != nil || !ok || got != crc {
		t.Fatalf("SeedFileCRC(%d): SettleFileCRC = %#x, %v, %v; want %#x, true, nil", fileIdx, got, ok, err, crc)
	}
}
