package job

import (
	"hash/crc32"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// zeroLengthRows is file 0 of verifiedTestJob with article 1 empty: three
// 100-byte articles around a zero-length one, whose row is CRC 0 at offset 100.
func zeroLengthRows(data []byte) []durability.WrittenRow {
	return []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(data[0:100])},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 0, CRC32: 0},
		{FileIdx: 0, ArtIdx: 2, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(data[100:200])},
		{FileIdx: 0, ArtIdx: 3, Offset: 200, Length: 100, CRC32: crc32.ChecksumIEEE(data[200:300])},
	}
}

// TestInstallCompleteFile_KeepsAZeroLengthRow: the live door accepts a
// zero-length article's row, so the same persisted rows must install after a
// restart with the same whole-file CRC, nothing dropped and no article failed.
func TestInstallCompleteFile_KeepsAZeroLengthRow(t *testing.T) {
	t.Parallel()
	data := chainData()[:300]
	rows := zeroLengthRows(data)

	live := verifiedTestJob(t)
	for _, r := range rows {
		if err := live.MarkArticleWritten(r); err != nil {
			t.Fatalf("MarkArticleWritten: %v", err)
		}
	}
	liveCRC, liveOK, err := live.SettleFileCRC(0)
	if err != nil || !liveOK || liveCRC != crc32.ChecksumIEEE(data) {
		t.Fatalf("live settle = %08x, %v, %v; want %08x, true, nil", liveCRC, liveOK, err, crc32.ChecksumIEEE(data))
	}

	restarted := verifiedTestJob(t)
	dropped, err := restarted.InstallCompleteFile(0, rows)
	if err != nil || dropped != 0 {
		t.Fatalf("InstallCompleteFile = %d, %v; want 0, nil", dropped, err)
	}
	p := restarted.Progress()
	if got := p.FileAssembledCRC32(0); got != liveCRC {
		t.Errorf("restart CRC = %08x, live CRC = %08x; they must agree", got, liveCRC)
	}
	for art := range 4 {
		if p.ArticleFailed(art) || !p.ArticleDone(art) {
			t.Errorf("article %d after restart: done=%v failed=%v, want done and not failed",
				art, p.ArticleDone(art), p.ArticleFailed(art))
		}
	}
}

// TestInstallVerified_KeepsAZeroLengthRow is the complete=0 half: the verified
// zero-length row installs Done and stays resident for the CRC.
func TestInstallVerified_KeepsAZeroLengthRow(t *testing.T) {
	t.Parallel()
	rows := zeroLengthRows(chainData()[:300])
	j := verifiedTestJob(t)
	dropped, err := j.InstallVerified(0, rows)
	if err != nil || dropped != 0 {
		t.Fatalf("InstallVerified = %d, %v; want 0, nil", dropped, err)
	}
	if !j.Progress().ArticleDone(1) {
		t.Error("the zero-length article is not Done after a restart")
	}
	if got := j.FileRows(0); len(got) != len(rows) {
		t.Errorf("FileRows(0) has %d rows, want all %d", len(got), len(rows))
	}
}
