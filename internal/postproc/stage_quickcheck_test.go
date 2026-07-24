package postproc

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// buildQCJob builds a single-file *queue.Job whose file has a resolved
// filename, byte count, and assembled CRC32 already set — the shape
// verifyJobCRCs reads via Manifest/Progress accessors.
func buildQCJob(t *testing.T, id, filename string, bytes int64, crc uint32) *queue.Job {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "subject.bin", Bytes: bytes, Articles: []nzb.Article{{ID: id + "-a@t", Bytes: int(bytes), Number: 1}}},
	}}
	qjob, err := queue.NewJob(parsed, queue.AddOptions{Filename: id + ".nzb", Name: id}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.SetFileFilename(qjob.ID, 0, filename); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	if err := q.SetFileCRC32(qjob.ID, 0, crc); err != nil {
		t.Fatalf("SetFileCRC32: %v", err)
	}
	return q.SnapshotJob(qjob.ID)
}

var (
	typeMain     = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'M', 'a', 'i', 'n', 0x00, 0x00, 0x00, 0x00}
	typeFileDesc = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'}
	typeIFSC     = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'I', 'F', 'S', 'C', 0x00, 0x00, 0x00, 0x00}
)

func buildPacket(setID [16]byte, pktType [16]byte, body []byte) []byte {
	packetLen := uint64(64 + len(body))
	buf := make([]byte, packetLen)
	copy(buf[0:8], "PAR2\x00PKT")
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)
	copy(buf[32:48], setID[:])
	copy(buf[48:64], pktType[:])
	copy(buf[64:], body)

	h := md5.New()
	h.Write(buf[32:48])
	h.Write(buf[48:64])
	h.Write(body)
	copy(buf[16:32], h.Sum(nil))

	return buf
}

func buildMainBody(sliceSize uint64, fileIDs ...[16]byte) []byte {
	body := make([]byte, 12+len(fileIDs)*16)
	binary.LittleEndian.PutUint64(body[0:8], sliceSize)
	// Leave recoverySetCount (body[8:12]) as 0.
	for i, id := range fileIDs {
		copy(body[12+i*16:12+(i+1)*16], id[:])
	}
	return body
}

func buildFileDescBody(fileID [16]byte, fullHash [16]byte, hash16k [16]byte, fileSize uint64, fileName string) []byte {
	nameBytes := []byte(fileName)
	pad := (4 - (len(nameBytes) % 4)) % 4
	body := make([]byte, 16+16+16+8, 16+16+16+8+len(nameBytes)+pad)
	copy(body[0:16], fileID[:])
	copy(body[16:32], fullHash[:])
	copy(body[32:48], hash16k[:])
	binary.LittleEndian.PutUint64(body[48:56], fileSize)
	body = append(body, nameBytes...)
	for range pad {
		body = append(body, 0)
	}
	return body
}

func buildIFSCBody(fileID [16]byte, sliceMD5 [16]byte, sliceCRC uint32) []byte {
	body := make([]byte, 16+20)
	copy(body[0:16], fileID[:])
	copy(body[16:32], sliceMD5[:])
	binary.LittleEndian.PutUint32(body[32:36], sliceCRC)
	return body
}

func TestQuickCheckStage_Disabled(t *testing.T) {
	_ = (*QuickCheckStage).verifyJobCRCs

	stage := NewQuickCheckStage()
	stage.SetEnabled(false)

	job := &Job{
		Queue: newQueueJob(t, "test-disabled", 0),
	}

	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	linesStr := strings.Join(job.OutputLines, "\n")
	if !strings.Contains(linesStr, "Disabled — par2 repair will run") {
		t.Errorf("expected disabled log message, got: %v", job.OutputLines)
	}
}

func TestQuickCheckStage_NoPar2(t *testing.T) {
	stage := NewQuickCheckStage()
	stage.SetEnabled(true)

	job := &Job{
		Queue:       newQueueJob(t, "test-no-par2", 0),
		DownloadDir: t.TempDir(),
	}

	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	linesStr := strings.Join(job.OutputLines, "\n")
	if !strings.Contains(linesStr, "No par2 files found") {
		t.Errorf("expected no par2 files log message, got: %v", job.OutputLines)
	}
}

func TestQuickCheckStage_Run(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{1, 2, 3, 4}
	fileID := [16]byte{10, 20, 30}
	fullHash := [16]byte{0xAA, 0xBB, 0xCC}

	content := []byte("flat file content for quickcheck stage test")
	hash16k := md5.Sum(content)
	fileName := "Subdir/original.txt"

	sliceSize := uint64(len(content))

	// Build par2 file packets.
	mainPkt := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))
	fdPkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, sliceSize, fileName))
	ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))

	parContent := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
	parContent = append(parContent, mainPkt...)
	parContent = append(parContent, fdPkt...)
	parContent = append(parContent, ifscPkt...)

	if err := os.WriteFile(parPath, parContent, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create flat file.
	flatName := "Subdir_original.txt"
	if err := os.WriteFile(filepath.Join(tmpDir, flatName), content, 0o644); err != nil {
		t.Fatal(err)
	}

	stage := NewQuickCheckStage()
	stage.SetEnabled(true)

	job := &Job{
		Queue:       buildQCJob(t, "job-qc", "original.txt", int64(len(content)), crc32.ChecksumIEEE(content)),
		DownloadDir: tmpDir,
	}

	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if !job.QuickCheckPassed {
		t.Error("expected job.QuickCheckPassed to be true")
	}

	linesStr := strings.Join(job.OutputLines, "\n")
	if !strings.Contains(linesStr, "Subdir_original.txt → Subdir/original.txt") {
		t.Errorf("expected relocation line in output, got: %s", linesStr)
	}
	if !strings.Contains(linesStr, "1/1 par2-tracked files verified OK") {
		t.Errorf("expected CRC OK verification line in output, got: %s", linesStr)
	}

	wantVerifiedLine := fmt.Sprintf("%s: CRC verified (%08x)", "original.txt", crc32.ChecksumIEEE(content))
	if !strings.Contains(linesStr, wantVerifiedLine) {
		t.Errorf("expected per-file verified line %q in output, got: %s", wantVerifiedLine, linesStr)
	}
}

func TestQuickCheckStage_CRCErrors(t *testing.T) {
	t.Run("CRC Mismatch", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		setID := [16]byte{1, 2, 3, 4}
		fileID := [16]byte{10, 20, 30}
		fullHash := [16]byte{0xAA, 0xBB, 0xCC}

		content := []byte("flat file content for quickcheck stage test")
		hash16k := md5.Sum(content)
		fileName := "original.txt"

		sliceSize := uint64(len(content))

		mainPkt := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))
		fdPkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, sliceSize, fileName))
		ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))

		parContent := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
		parContent = append(parContent, mainPkt...)
		parContent = append(parContent, fdPkt...)
		parContent = append(parContent, ifscPkt...)

		if err := os.WriteFile(parPath, parContent, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := os.WriteFile(filepath.Join(tmpDir, fileName), content, 0o644); err != nil {
			t.Fatal(err)
		}

		stage := NewQuickCheckStage()
		stage.SetEnabled(true)

		job := &Job{
			Queue:       buildQCJob(t, "job-qc-err", "original.txt", int64(len(content)), crc32.ChecksumIEEE(content)+1), // Mismatch!
			DownloadDir: tmpDir,
		}

		err := stage.Run(t.Context(), job)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if job.QuickCheckPassed {
			t.Error("expected job.QuickCheckPassed to be false on CRC mismatch")
		}

		linesStr := strings.Join(job.OutputLines, "\n")
		if !strings.Contains(linesStr, "CRC mismatch") {
			t.Errorf("expected CRC mismatch line, got: %s", linesStr)
		}
	})

	t.Run("No CRC", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		setID := [16]byte{1, 2, 3, 4}
		fileID := [16]byte{10, 20, 30}
		fullHash := [16]byte{0xAA, 0xBB, 0xCC}

		content := []byte("flat file content for quickcheck stage test")
		hash16k := md5.Sum(content)
		fileName := "original.txt"

		sliceSize := uint64(len(content))

		mainPkt := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))
		fdPkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, sliceSize, fileName))
		ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))

		parContent := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
		parContent = append(parContent, mainPkt...)
		parContent = append(parContent, fdPkt...)
		parContent = append(parContent, ifscPkt...)

		if err := os.WriteFile(parPath, parContent, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := os.WriteFile(filepath.Join(tmpDir, fileName), content, 0o644); err != nil {
			t.Fatal(err)
		}

		stage := NewQuickCheckStage()
		stage.SetEnabled(true)

		job := &Job{
			Queue:       buildQCJob(t, "job-qc-nocrc", "original.txt", int64(len(content)), 0), // No CRC!
			DownloadDir: tmpDir,
		}

		err := stage.Run(t.Context(), job)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if job.QuickCheckPassed {
			t.Error("expected job.QuickCheckPassed to be false on missing CRC")
		}

		linesStr := strings.Join(job.OutputLines, "\n")
		if !strings.Contains(linesStr, "CRC unavailable") {
			t.Errorf("expected CRC unavailable line, got: %s", linesStr)
		}
	})

	t.Run("Unverified", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		setID := [16]byte{1, 2, 3, 4}
		fileID := [16]byte{10, 20, 30}
		fullHash := [16]byte{0xAA, 0xBB, 0xCC}

		content := []byte("flat file content for quickcheck stage test")
		hash16k := md5.Sum(content)
		fileName := "original.txt"

		sliceSize := uint64(len(content))

		mainPkt := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))
		fdPkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, sliceSize, fileName))
		ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))

		parContent := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
		parContent = append(parContent, mainPkt...)
		parContent = append(parContent, fdPkt...)
		parContent = append(parContent, ifscPkt...)

		if err := os.WriteFile(parPath, parContent, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		// The downloaded file's name and content don't match the par2 entry
		// (original.txt) at all, and CRC+size fallback can't rescue it either
		// — so the par2 entry is left unconsumed: a genuine name mismatch.
		stage := NewQuickCheckStage()
		stage.SetEnabled(true)

		job := &Job{
			Queue:       buildQCJob(t, "job-qc-unver", "unrelated.dat", 999, 0x99999999),
			DownloadDir: tmpDir,
		}

		err := stage.Run(t.Context(), job)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if job.QuickCheckPassed {
			t.Error("expected job.QuickCheckPassed to be false on unverified files")
		}

		linesStr := strings.Join(job.OutputLines, "\n")
		if !strings.Contains(linesStr, fileName) {
			t.Errorf("expected OutputLines to name the unverified file %q, got: %s", fileName, linesStr)
		}
		if !strings.Contains(linesStr, "not found by name") {
			t.Errorf("expected OutputLines to explain why, got: %s", linesStr)
		}
	})
}
