package app

// Cold-read harness for verifyJobFiles. It answers one question: how long does
// the verifier take to read back a 4 GiB file whose pages are NOT in the page
// cache, on the filesystem under test (an NFS mount)?
//
// Both tests skip unless GONZBD_VERIFY_COLD_DIR names a directory. Run them as
// two separate invocations with the page cache dropped in between, otherwise
// the prepare step leaves the file hot and the measure step times RAM:
//
//	GONZBD_VERIFY_COLD_DIR=/mnt/nas/public/usenet/coldread-bench \
//	    go test ./internal/app/ -run '^TestVerifyColdRead_Prepare$' -v -count=1 -timeout 30m
//	echo 3 | sudo tee /proc/sys/vm/drop_caches
//	GONZBD_VERIFY_COLD_DIR=/mnt/nas/public/usenet/coldread-bench \
//	    go test ./internal/app/ -run '^TestVerifyColdRead_Measure$' -v -count=1 -timeout 30m
//
// The measure step prints one line: COLD_READ seconds=<x> MiB/s=<y>.

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"

	"math/rand/v2"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

const (
	coldDirEnv   = "GONZBD_VERIFY_COLD_DIR"
	coldFileName = "cold.bin"
	coldFileSize = int64(4 << 30)
	coldArtSize  = int64(750_000)
	// coldArtCount is ceil(4 GiB / 750000): the last article is short.
	coldArtCount = int((coldFileSize + coldArtSize - 1) / coldArtSize)
)

func coldDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(coldDirEnv)
	if dir == "" {
		t.Skipf("%s is not set", coldDirEnv)
	}
	return dir
}

func coldArtLen(i int) int64 {
	return min(coldArtSize, coldFileSize-int64(i)*coldArtSize)
}

// fillColdArticle writes article i's deterministic content into b[:coldArtLen(i)]
// and returns that slice.
func fillColdArticle(i int, b []byte) []byte {
	var seed [32]byte
	for k := range 8 {
		seed[k] = byte(uint64(i) >> (8 * k)) //nolint:gosec // G115: low bytes of a non-negative index
	}
	b = b[:coldArtLen(i)]
	_, _ = rand.NewChaCha8(seed).Read(b) // ChaCha8.Read never fails
	return b
}

func TestVerifyColdRead_Prepare(t *testing.T) {
	dir := coldDir(t)
	f, err := os.Create(filepath.Join(dir, coldFileName)) //nolint:gosec // G304: operator-supplied bench directory
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()

	start := time.Now()
	w := bufio.NewWriterSize(f, 1<<20)
	buf := make([]byte, coldArtSize)
	for i := range coldArtCount {
		if _, err := w.Write(fillColdArticle(i, buf)); err != nil {
			t.Fatalf("write article %d: %v", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("fsync: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	el := time.Since(start)
	t.Logf("wrote %d articles, %d bytes in %.1fs (%.1f MiB/s)",
		coldArtCount, coldFileSize, el.Seconds(), float64(coldFileSize)/(1<<20)/el.Seconds())
}

func TestVerifyColdRead_Measure(t *testing.T) {
	dir := coldDir(t)

	arts := make([]job.JobArticle, coldArtCount)
	rows := make([]durability.WrittenRow, coldArtCount)
	buf := make([]byte, coldArtSize)
	for i := range arts {
		n := coldArtLen(i)
		arts[i] = job.JobArticle{ID: fmt.Sprintf("<c%d@t>", i), Bytes: int(n), Number: i + 1}
		rows[i] = durability.WrittenRow{
			FileIdx: 0, ArtIdx: int32(i), Offset: int64(i) * coldArtSize, Length: n, //nolint:gosec // G115: bounded by coldArtCount
			CRC32: crc32.ChecksumIEEE(fillColdArticle(i, buf)),
		}
	}
	m := job.NewManifest([]job.JobFile{{Subject: coldFileName, Bytes: coldFileSize, Articles: arts}})
	files := []durability.FileRow{{FileIndex: 0, Filename: coldFileName}}
	start := time.Now()
	res, err := verifyJobFiles(t.Context(), m, files, rows, in(dir), false)
	el := time.Since(start)
	if err != nil {
		t.Fatalf("verifyJobFiles: %v", err)
	}

	finished := 0
	for _, v := range res.Verdicts {
		if v.SetComplete {
			finished++
		}
	}
	mibps := float64(coldFileSize) / (1 << 20) / el.Seconds()
	t.Logf("verified %d of %d rows, %d files finished, in %.2fs (%.1f MiB/s)",
		len(res.Verified[0]), len(rows), finished, el.Seconds(), mibps)
	fmt.Printf("COLD_READ seconds=%.2f MiB/s=%.1f\n", el.Seconds(), mibps)
	if len(res.Verified[0]) != len(rows) {
		t.Errorf("verified %d rows, want %d", len(res.Verified[0]), len(rows))
	}
	if len(res.Failed) != 0 || len(res.Verdicts) != finished {
		t.Errorf("unexpected deletions: failed=%v verdicts=%+v", res.Failed, res.Verdicts)
	}
}
