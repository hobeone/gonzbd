package par2

import (
	"bytes"
	"crypto/md5" //nolint:gosec // md5 used for PAR2 spec packet integrity
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

// buildPacket creates a complete PAR2 packet with valid MD5 in the header.
// setID and packetType go into the header; body is the packet body.
func buildPacket(setID [16]byte, packetType [16]byte, body []byte) []byte {
	packetLen := uint64(64 + len(body))
	buf := make([]byte, packetLen)

	// Magic.
	copy(buf[0:8], magic)
	// Packet length.
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)
	// ParsedSet ID.
	copy(buf[32:48], setID[:])
	// Packet type.
	copy(buf[48:64], packetType[:])
	// Body.
	copy(buf[64:], body)

	// Compute MD5 over (setID + type + body).
	h := md5.New() //nolint:gosec
	h.Write(buf[32:48])
	h.Write(buf[48:64])
	h.Write(body)
	copy(buf[16:32], h.Sum(nil))

	return buf
}

// buildFileDescBody creates a FileDesc packet body.
func buildFileDescBody(fileID, fullHash, hash16k [16]byte, fileSize uint64, fileName string) []byte {
	nameBytes := []byte(fileName)
	// Pad to 4-byte boundary.
	padding := (4 - (len(nameBytes) % 4)) % 4
	nameBytes = append(nameBytes, make([]byte, padding)...)

	body := make([]byte, 56+len(nameBytes))
	copy(body[0:16], fileID[:])
	copy(body[16:32], fullHash[:])
	copy(body[32:48], hash16k[:])
	binary.LittleEndian.PutUint64(body[48:56], fileSize)
	copy(body[56:], nameBytes)
	return body
}

// buildMainBody creates a Main packet body with sliceSize and recovery set file IDs.
func buildMainBody(sliceSize uint64, fileIDs ...[16]byte) []byte {
	body := make([]byte, 8+len(fileIDs)*16)
	binary.LittleEndian.PutUint64(body[0:8], sliceSize)
	for i, id := range fileIDs {
		copy(body[8+i*16:8+(i+1)*16], id[:])
	}
	return body
}

func TestParsePar2SetWithOptions(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	var setID, fileID, fullHash, hash16k [16]byte
	setID[0] = 0xAA
	fileID[0] = 0xBB

	mainPkt := buildPacket(setID, typeMain, buildMainBody(16384, fileID))
	filePkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, 1024, "file1.txt"))

	content := append(mainPkt, filePkt...)
	if err := os.WriteFile(parPath, content, 0644); err != nil {
		t.Fatalf("failed to write par2 file: %v", err)
	}

	// 1. With custom body limit (e.g. 65536 bytes) on a packet body of 70000 bytes, parsing should fail.
	largeBodyPkt := buildPacket(setID, typeRecovery, make([]byte, 70000))
	largeParPath := filepath.Join(tmpDir, "large.par2")
	if err := os.WriteFile(largeParPath, largeBodyPkt, 0644); err != nil {
		t.Fatalf("failed to write large par2 file: %v", err)
	}
	lowOpts := DefaultParseOptions()
	lowOpts.MaxPacketBodySize = 65536
	_, err := ParsePar2SetWithOptions(largeParPath, lowOpts)
	if err == nil {
		t.Fatalf("expected error when body exceeds MaxPacketBodySize = 65536, got nil")
	}

	// 2. With default options, parsing should succeed.
	set, err := ParsePar2SetWithOptions(parPath, DefaultParseOptions())
	if err != nil {
		t.Fatalf("ParsePar2SetWithOptions with DefaultParseOptions failed: %v", err)
	}
	if len(set.Files) != 1 {
		t.Errorf("expected 1 file, got %d", len(set.Files))
	}
}

// buildMainBodyWithCount creates a spec-correct Main packet body:
// sliceSize(8) + recoverySetCount(4) + N×16-byte FileID entries. Unlike
// buildMainBody (which omits the recoverySetCount field for brevity), this
// produces the exact body length the parser uses to derive expectedFiles —
// needed by tests that exercise the §4.5 early-exit optimization.
func buildMainBodyWithCount(sliceSize uint64, recoverySetCount uint32, fileIDs ...[16]byte) []byte {
	body := make([]byte, 12+len(fileIDs)*16)
	binary.LittleEndian.PutUint64(body[0:8], sliceSize)
	binary.LittleEndian.PutUint32(body[8:12], recoverySetCount)
	for i, id := range fileIDs {
		copy(body[12+i*16:12+(i+1)*16], id[:])
	}
	return body
}

// buildIFSCBody creates an IFSC packet body with fileID and slice CRC entries.
func buildIFSCBody(fileID [16]byte, slices []ifscSlice) []byte {
	body := make([]byte, 16+len(slices)*20)
	copy(body[0:16], fileID[:])
	for i, s := range slices {
		off := 16 + i*20
		copy(body[off:off+16], s.md5Hash[:])
		binary.LittleEndian.PutUint32(body[off+16:off+20], s.crc32)
	}
	return body
}

func TestParseFileDescriptions(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{1, 2, 3, 4}
	fileID := [16]byte{10, 20, 30}
	fullHash := [16]byte{0xAA, 0xBB, 0xCC}
	hash16k := [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	fileName := "original.mkv"

	body := buildFileDescBody(fileID, fullHash, hash16k, 123456, fileName)
	pkt := buildPacket(setID, typeFileDesc, body)

	if err := os.WriteFile(parPath, pkt, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ParseFileDescriptions(parPath)
	if err != nil {
		t.Fatalf("ParseFileDescriptions: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d files, want 1", len(got))
	}

	fd := got[0]
	if fd.FileName != fileName {
		t.Errorf("FileName = %q, want %q", fd.FileName, fileName)
	}
	if fd.Hash16k != hash16k {
		t.Errorf("Hash16k = %x, want %x", fd.Hash16k, hash16k)
	}
	if fd.FileSize != 123456 {
		t.Errorf("FileSize = %d, want 123456", fd.FileSize)
	}
	if fd.FileID != fileID {
		t.Errorf("FileID = %x, want %x", fd.FileID, fileID)
	}
	if fd.FullHash != fullHash {
		t.Errorf("FullHash = %x, want %x", fd.FullHash, fullHash)
	}
}

func TestParseFileDescriptions_MalformedLength(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	packetLen := uint64(1024 * 1024 * 1024) // 1GB
	buf := make([]byte, 64)
	copy(buf[0:8], magic)
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)

	if err := os.WriteFile(parPath, buf, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ParseFileDescriptions(parPath)
	if err == nil {
		t.Fatal("ParseFileDescriptions expected error on massive packetLen, got nil")
	}
}

func TestParsePar2Set_MainPacket(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{0x01, 0x02, 0x03, 0x04}
	fileID := [16]byte{0xAA}

	mainBody := buildMainBody(768*1024, fileID)
	mainPkt := buildPacket(setID, typeMain, mainBody)

	if err := os.WriteFile(parPath, mainPkt, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	set, err := ParsePar2Set(parPath)
	if err != nil {
		t.Fatalf("ParsePar2Set: %v", err)
	}

	if set.SetID != setID {
		t.Errorf("SetID = %x, want %x", set.SetID, setID)
	}
	if set.SliceSize != 768*1024 {
		t.Errorf("SliceSize = %d, want %d", set.SliceSize, 768*1024)
	}
}

// TestParsePar2Set_PacketLengthBoundary exercises the `packetLen < 64`
// boundary directly: 64 is the minimum valid packet length (a zero-length
// body), so 63 must be rejected and 64 accepted.
func TestParsePar2Set_PacketLengthBoundary(t *testing.T) {
	t.Run("packetLen below header size is rejected", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		buf := make([]byte, 64)
		copy(buf[0:8], magic)
		binary.LittleEndian.PutUint64(buf[8:16], 63) // < 64: shorter than the header itself

		if err := os.WriteFile(parPath, buf, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if _, err := ParsePar2Set(parPath); err == nil {
			t.Fatal("ParsePar2Set: expected error for packetLen=63, got nil")
		}
	})

	t.Run("packetLen exactly at header size is accepted", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		setID := [16]byte{0x07}
		pkt := buildPacket(setID, typeCreator, []byte{}) // packetLen = 64 + 0 = 64

		if err := os.WriteFile(parPath, pkt, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		set, err := ParsePar2Set(parPath)
		if err != nil {
			t.Fatalf("ParsePar2Set: expected packetLen=64 to be accepted, got error: %v", err)
		}
		if set.Creator != "" {
			t.Errorf("Creator = %q, want empty for zero-length body", set.Creator)
		}
	})
}

// TestParsePar2Set_MainBodyLengthBoundary exercises the `len(body) < 8`
// boundary in the Main-packet branch: bodies shorter than 8 bytes can't
// hold the sliceSize field and must be skipped, while exactly 8 bytes is
// the minimal valid Main body.
func TestParsePar2Set_MainBodyLengthBoundary(t *testing.T) {
	setID := [16]byte{0x08}

	t.Run("body shorter than 8 bytes is skipped", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		shortBody := []byte{1, 2, 3, 4, 5, 6, 7} // 7 bytes — can't hold a uint64 sliceSize
		pkt := buildPacket(setID, typeMain, shortBody)

		if err := os.WriteFile(parPath, pkt, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		set, err := ParsePar2Set(parPath)
		if err != nil {
			t.Fatalf("ParsePar2Set: %v", err)
		}
		if set.SliceSize != 0 {
			t.Errorf("SliceSize = %d, want 0 (short Main body must be skipped)", set.SliceSize)
		}
	})

	t.Run("body of exactly 8 bytes is parsed", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		body := make([]byte, 8)
		binary.LittleEndian.PutUint64(body, 999999)
		pkt := buildPacket(setID, typeMain, body)

		if err := os.WriteFile(parPath, pkt, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		set, err := ParsePar2Set(parPath)
		if err != nil {
			t.Fatalf("ParsePar2Set: %v", err)
		}
		if set.SliceSize != 999999 {
			t.Errorf("SliceSize = %d, want 999999", set.SliceSize)
		}
	})
}

// TestParsePar2Set_EarlyExitOptimization exercises the §4.5 scan optimization
// (parser.go: "expectedFiles > 0 && fileSize > earlyExitThreshold && ...").
// It proves both that the optimization fires (skipping a trailing FileDesc
// packet once the expected count is reached in a file above the 10 MiB
// threshold) and that it does not fire for files below the threshold.
func TestParsePar2Set_EarlyExitOptimization(t *testing.T) {
	setID := [16]byte{0x09}
	fileID1 := [16]byte{0x01}
	fileID2 := [16]byte{0x02}
	fullHash := [16]byte{0xAA}
	hash16k := [16]byte{0xBB}

	build := func(t *testing.T, paddingSize int) *ParsedSet {
		t.Helper()
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		// recoverySetCount=1 with one FileID makes expectedFiles == 1, so a
		// single FileDesc + IFSC pair satisfies the "all expected packets seen"
		// half of the early-exit condition.
		mainBody := buildMainBodyWithCount(768*1024, 1, fileID1)

		mainPkt := buildPacket(setID, typeMain, mainBody)
		fd1Pkt := buildPacket(setID, typeFileDesc,
			buildFileDescBody(fileID1, fullHash, hash16k, 1000, "first.bin"))
		ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID1, nil))
		recPkt := buildPacket(setID, typeRecovery, make([]byte, paddingSize))
		fd2Pkt := buildPacket(setID, typeFileDesc,
			buildFileDescBody(fileID2, fullHash, hash16k, 2000, "second.bin"))

		buf := make([]byte, 0, len(mainPkt)+len(fd1Pkt)+len(ifscPkt)+len(recPkt)+len(fd2Pkt))
		buf = append(buf, mainPkt...)
		buf = append(buf, fd1Pkt...)
		buf = append(buf, ifscPkt...)
		buf = append(buf, recPkt...)
		buf = append(buf, fd2Pkt...)

		if err := os.WriteFile(parPath, buf, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		set, err := ParsePar2Set(parPath)
		if err != nil {
			t.Fatalf("ParsePar2Set: %v", err)
		}
		return set
	}

	t.Run("file above threshold stops once expected packets are seen", func(t *testing.T) {
		// Total size must exceed earlyExitThreshold (10 MiB) for the
		// optimization to engage; the trailing FileDesc must then be skipped.
		set := build(t, 11*1024*1024)
		if len(set.Files) != 1 {
			t.Fatalf("got %d files, want 1 (early exit should skip the trailing FileDesc)", len(set.Files))
		}
		if set.Files[0].FileID != fileID1 {
			t.Errorf("FileID = %x, want %x", set.Files[0].FileID, fileID1)
		}
	})

	t.Run("file below threshold keeps scanning to the end", func(t *testing.T) {
		set := build(t, 1024)
		if len(set.Files) != 2 {
			t.Fatalf("got %d files, want 2 (file below threshold must not early-exit)", len(set.Files))
		}
	})
}

func TestParsePar2Set_MD5Validation(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{1}
	hash16k := [16]byte{0xDE, 0xAD}

	// Build a valid FileDesc packet, then corrupt its MD5.
	body := buildFileDescBody([16]byte{10}, [16]byte{}, hash16k, 1000, "good.txt")
	pkt := buildPacket(setID, typeFileDesc, body)

	// Corrupt the MD5 in header[16:32].
	pkt[16] ^= 0xFF

	if err := os.WriteFile(parPath, pkt, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	set, err := ParsePar2Set(parPath)
	if err != nil {
		t.Fatalf("ParsePar2Set: %v", err)
	}

	// The packet should have been dropped due to MD5 mismatch.
	if len(set.Files) != 0 {
		t.Errorf("expected 0 files (MD5 mismatch should drop packet), got %d", len(set.Files))
	}
}

func TestParsePar2Set_IFSCAndCRC(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{1}
	fileID := [16]byte{0x42}
	sliceSize := uint64(1024)
	fileSize := uint64(2048) // exactly 2 full slices

	// Main packet.
	mainBody := buildMainBody(sliceSize, fileID)
	mainPkt := buildPacket(setID, typeMain, mainBody)

	// FileDesc packet.
	fdBody := buildFileDescBody(fileID, [16]byte{}, [16]byte{0xAA}, fileSize, "movie.mkv")
	fdPkt := buildPacket(setID, typeFileDesc, fdBody)

	// IFSC packet with 2 slices.
	slices := []ifscSlice{
		{crc32: 0x12345678},
		{crc32: 0xABCDEF01},
	}
	ifscBody := buildIFSCBody(fileID, slices)
	ifscPkt := buildPacket(setID, typeIFSC, ifscBody)

	// Write all packets.
	buf := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
	buf = append(buf, mainPkt...)
	buf = append(buf, fdPkt...)
	buf = append(buf, ifscPkt...)

	if err := os.WriteFile(parPath, buf, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	set, err := ParsePar2Set(parPath)
	if err != nil {
		t.Fatalf("ParsePar2Set: %v", err)
	}

	if len(set.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(set.Files))
	}
	if set.Files[0].FileCRC32 == 0 {
		t.Error("expected non-zero FileCRC32 after IFSC reconstruction")
	}
	if set.SliceSize != sliceSize {
		t.Errorf("SliceSize = %d, want %d", set.SliceSize, sliceSize)
	}
}

func TestParsePar2Set_Deduplication(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{1}
	sharedHash16k := [16]byte{0xDE, 0xAD, 0xBE, 0xEF}

	// Two files with the same Hash16k.
	fd1Body := buildFileDescBody([16]byte{1}, [16]byte{}, sharedHash16k, 1000, "file1.txt")
	fd1Pkt := buildPacket(setID, typeFileDesc, fd1Body)

	fd2Body := buildFileDescBody([16]byte{2}, [16]byte{}, sharedHash16k, 2000, "file2.txt")
	fd2Pkt := buildPacket(setID, typeFileDesc, fd2Body)

	// A third file with a unique Hash16k.
	uniqueHash16k := [16]byte{0xFF, 0xEE}
	fd3Body := buildFileDescBody([16]byte{3}, [16]byte{}, uniqueHash16k, 3000, "unique.txt")
	fd3Pkt := buildPacket(setID, typeFileDesc, fd3Body)

	buf := make([]byte, 0, len(fd1Pkt)+len(fd2Pkt)+len(fd3Pkt))
	buf = append(buf, fd1Pkt...)
	buf = append(buf, fd2Pkt...)
	buf = append(buf, fd3Pkt...)

	if err := os.WriteFile(parPath, buf, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	set, err := ParsePar2Set(parPath)
	if err != nil {
		t.Fatalf("ParsePar2Set: %v", err)
	}

	if len(set.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(set.Files))
	}

	// Both files sharing Hash16k should be HasDuplicate=true.
	for _, fd := range set.Files {
		if fd.Hash16k == sharedHash16k && !fd.HasDuplicate {
			t.Errorf("file %q with shared Hash16k should have HasDuplicate=true", fd.FileName)
		}
		if fd.Hash16k == uniqueHash16k && fd.HasDuplicate {
			t.Errorf("file %q with unique Hash16k should have HasDuplicate=false", fd.FileName)
		}
	}

	// By16k should contain only the unique file.
	if len(set.By16k) != 1 {
		t.Errorf("By16k has %d entries, want 1 (only the unique file)", len(set.By16k))
	}
	if _, ok := set.By16k[uniqueHash16k]; !ok {
		t.Error("By16k should contain the unique Hash16k entry")
	}
	if _, ok := set.By16k[sharedHash16k]; ok {
		t.Error("By16k should NOT contain the shared/duplicate Hash16k entry")
	}
}

func TestParsePar2Set_JunkByteScan(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{5}
	fileID := [16]byte{0x99}
	hash16k := [16]byte{0xBB}

	fdBody := buildFileDescBody(fileID, [16]byte{}, hash16k, 500, "found_after_junk.txt")
	pkt := buildPacket(setID, typeFileDesc, fdBody)

	// Prepend 128 bytes of junk before the valid packet.
	junk := make([]byte, 128)
	for i := range junk {
		junk[i] = 0xFE
	}

	buf := make([]byte, 0, len(junk)+len(pkt))
	buf = append(buf, junk...)
	buf = append(buf, pkt...)

	if err := os.WriteFile(parPath, buf, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	set, err := ParsePar2Set(parPath)
	if err != nil {
		t.Fatalf("ParsePar2Set: %v", err)
	}

	if len(set.Files) != 1 {
		t.Fatalf("expected 1 file after junk scan, got %d", len(set.Files))
	}
	if set.Files[0].FileName != "found_after_junk.txt" {
		t.Errorf("FileName = %q, want %q", set.Files[0].FileName, "found_after_junk.txt")
	}
}

func TestParsePar2Set_RecoveryAndCreator(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{0x07}

	// Recovery packets (body content doesn't matter, we just count them).
	rec1 := buildPacket(setID, typeRecovery, make([]byte, 100))
	rec2 := buildPacket(setID, typeRecovery, make([]byte, 200))

	// Creator packet.
	creatorText := "par2cmdline v0.8.1"
	padLen := (4 - (len(creatorText) % 4)) % 4
	creatorBody := append([]byte(creatorText), make([]byte, padLen)...)
	creatorPkt := buildPacket(setID, typeCreator, creatorBody)

	buf := make([]byte, 0, len(rec1)+len(rec2)+len(creatorPkt))
	buf = append(buf, rec1...)
	buf = append(buf, rec2...)
	buf = append(buf, creatorPkt...)

	if err := os.WriteFile(parPath, buf, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	set, err := ParsePar2Set(parPath)
	if err != nil {
		t.Fatalf("ParsePar2Set: %v", err)
	}

	if set.RecoveryBlocks != 2 {
		t.Errorf("RecoveryBlocks = %d, want 2", set.RecoveryBlocks)
	}
	if set.Creator != creatorText {
		t.Errorf("Creator = %q, want %q", set.Creator, creatorText)
	}
}

func TestParsePar2Set_MultiplePacketTypes(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{0xAA, 0xBB}
	fileID := [16]byte{0x42}
	sliceSize := uint64(4096)

	// Main packet.
	mainPkt := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))

	// FileDesc packet.
	fdPkt := buildPacket(setID, typeFileDesc,
		buildFileDescBody(fileID, [16]byte{0x11}, [16]byte{0x22}, 8192, "test.bin"))

	// Recovery packet.
	recPkt := buildPacket(setID, typeRecovery, make([]byte, 50))

	// Creator packet.
	creatorPkt := buildPacket(setID, typeCreator, []byte("GoNZBD\x00\x00"))

	buf := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(recPkt)+len(creatorPkt))
	buf = append(buf, mainPkt...)
	buf = append(buf, fdPkt...)
	buf = append(buf, recPkt...)
	buf = append(buf, creatorPkt...)

	if err := os.WriteFile(parPath, buf, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	set, err := ParsePar2Set(parPath)
	if err != nil {
		t.Fatalf("ParsePar2Set: %v", err)
	}

	if set.SetID != setID {
		t.Errorf("SetID = %x, want %x", set.SetID, setID)
	}
	if set.SliceSize != sliceSize {
		t.Errorf("SliceSize = %d, want %d", set.SliceSize, sliceSize)
	}
	if len(set.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(set.Files))
	}
	if set.Files[0].FileName != "test.bin" {
		t.Errorf("FileName = %q, want %q", set.Files[0].FileName, "test.bin")
	}
	if set.RecoveryBlocks != 1 {
		t.Errorf("RecoveryBlocks = %d, want 1", set.RecoveryBlocks)
	}
	if set.Creator != "GoNZBD" {
		t.Errorf("Creator = %q, want %q", set.Creator, "GoNZBD")
	}
}

func TestCorrectEncoding_UTF8(t *testing.T) {
	t.Parallel()
	// Pure ASCII passes through unchanged.
	got := correctEncoding([]byte("hello.txt"))
	if got != "hello.txt" {
		t.Errorf("got %q, want %q", got, "hello.txt")
	}
}

func TestCorrectEncoding_ValidUTF8_NFC(t *testing.T) {
	t.Parallel()
	// NFD form: 'ü' = U+0075 U+0308 (u + combining diaeresis)
	nfd := "M\xC3\xBCnchen.txt" // Already NFC 'ü'
	got := correctEncoding([]byte(nfd))
	if got != nfd {
		t.Errorf("got %q, want %q", got, nfd)
	}
}

func TestCorrectEncoding_ISO8859_1_Fallback(t *testing.T) {
	t.Parallel()
	// ISO-8859-1: 0xFC = 'ü', 0xE9 = 'é'
	// These bytes are NOT valid UTF-8, so should trigger Latin-1 decode.
	latin1 := []byte{'M', 0xFC, 'n', 'c', 'h', 'e', 'n', '.', 't', 'x', 't'}
	got := correctEncoding(latin1)
	want := "München.txt"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCorrectEncoding_ISO8859_1_AccentedE(t *testing.T) {
	t.Parallel()
	latin1 := []byte{'c', 'a', 'f', 0xE9, '.', 'n', 'z', 'b'}
	got := correctEncoding(latin1)
	want := "café.nzb"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseFileDescBody_ISO8859_1_FileName(t *testing.T) {
	t.Parallel()
	// Create a FileDesc body with ISO-8859-1 filename bytes.
	fileID := [16]byte{1}
	fullHash := [16]byte{2}
	hash16k := [16]byte{3}
	fileSize := uint64(1000)

	// ISO-8859-1 encoded "Ärger.txt" — 0xC4 = 'Ä' in Latin-1
	nameBytes := []byte{0xC4, 'r', 'g', 'e', 'r', '.', 't', 'x', 't', 0, 0, 0}

	body := make([]byte, 56+len(nameBytes))
	copy(body[0:16], fileID[:])
	copy(body[16:32], fullHash[:])
	copy(body[32:48], hash16k[:])
	binary.LittleEndian.PutUint64(body[48:56], fileSize)
	copy(body[56:], nameBytes)

	fd := parseFileDescBody(body)
	if fd == nil {
		t.Fatal("parseFileDescBody returned nil")
	}
	want := "Ärger.txt"
	if fd.FileName != want {
		t.Errorf("FileName = %q, want %q", fd.FileName, want)
	}
}

func TestScanForMagic(t *testing.T) {
	// Create a temp file containing some junk, then the magic header, then some more data.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "scan_test.bin")

	junk := make([]byte, 100)
	for i := range junk {
		junk[i] = 0xAA
	}

	content := append([]byte{}, junk...)
	content = append(content, magic...)
	content = append(content, 0x11, 0x22, 0x33)

	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	// Read first 64 bytes (simulating reading a header that isn't magic)
	hdr := make([]byte, 64)
	if _, err := io.ReadFull(f, hdr); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	found, err := scanForMagic(f, magic)
	if err != nil {
		t.Fatalf("scanForMagic error: %v", err)
	}
	if !found {
		t.Fatal("scanForMagic did not find magic")
	}

	// Verify cursor position. scanForMagic seeks to where magic starts (100).
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if pos != 100 {
		t.Errorf("cursor position = %d, want 100", pos)
	}
}

// countParsablePackets walks a PAR2 file packet-by-packet via the parser's own
// readNextPacket and returns how many valid packets it recovers. Counting raw
// packets (rather than the deduplicated ParsedSet) is what makes a single
// dropped packet observable: PAR2 files store redundant copies of critical
// packets, so losing one copy may not change the final set.
func countParsablePackets(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	header := make([]byte, 64)
	n := 0
	for {
		if _, _, err := readNextPacket(f, uint64(fi.Size()), header); err != nil {
			break
		}
		n++
	}
	return n
}

// TestScanForMagic_RecoversPacketAfterShortJunk proves the junk-skipping scan
// recovers a packet preceded by 1–7 bytes of junk. The magic is 8 bytes; after
// a 64-byte header read fails the magic check at offset P, the only ruled-out
// start is P itself, so the rescan must resume at P+1 (seek back len(header)-1 =
// 63). A seek back of 56 (= 64-8) instead resumes at P+8, skipping candidate
// starts P+1..P+7 and silently dropping a packet whenever 1–7 junk bytes
// precede it. Uses a real par2-generated file so the packet layout is genuine.
func TestScanForMagic_RecoversPacketAfterShortJunk(t *testing.T) {
	cleanPath := "../../test/fixtures/par2/data.par2"
	clean, err := os.ReadFile(cleanPath)
	if err != nil {
		t.Fatalf("read par2 fixture: %v", err)
	}

	cleanCount := countParsablePackets(t, cleanPath)
	if cleanCount < 2 {
		t.Fatalf("expected the par2 index to hold >=2 packets, got %d", cleanCount)
	}

	// Offset of the SECOND packet's magic (the first is at offset 0). We inject
	// junk immediately before it; packet 1 is read by its length field, so the
	// parser lands exactly here and must scan to recover packet 2.
	rel := bytes.Index(clean[len(magic):], magic)
	if rel < 0 {
		t.Fatalf("could not find a second PAR2 packet magic in the index file")
	}
	second := rel + len(magic)
	dir := t.TempDir()

	for k := 1; k <= 7; k++ {
		junked := make([]byte, 0, len(clean)+k)
		junked = append(junked, clean[:second]...)
		junked = append(junked, bytes.Repeat([]byte{0xFF}, k)...)
		junked = append(junked, clean[second:]...)

		junkPath := filepath.Join(dir, "junked.par2")
		if err := os.WriteFile(junkPath, junked, 0o644); err != nil {
			t.Fatalf("write junked par2: %v", err)
		}

		got := countParsablePackets(t, junkPath)
		if got != cleanCount {
			t.Errorf("with %d junk byte(s) before the 2nd packet: recovered %d packets, want %d (a packet was silently dropped)", k, got, cleanCount)
		}
	}
}

func TestValidatePacketMD5(t *testing.T) {
	setID := [16]byte{0x01, 0x02, 0x03}
	packetType := [16]byte{0xAA, 0xBB, 0xCC}
	body := []byte("hello validate")

	pkt := buildPacket(setID, packetType, body)
	header := pkt[:64]
	packetBody := pkt[64:]

	if !validatePacketMD5(header, packetBody) {
		t.Error("validatePacketMD5 failed on valid packet")
	}

	// Corrupt header MD5
	badHeader := append([]byte{}, header...)
	badHeader[16] ^= 0xFF
	if validatePacketMD5(badHeader, packetBody) {
		t.Error("validatePacketMD5 succeeded on corrupted header MD5")
	}
}

func TestParserUnexportedHelpersDirect(t *testing.T) {
	t.Parallel()

	// Direct references for alignment check
	_ = handlePacket
	_ = readNextPacket

	t.Run("parseIFSCBody short", func(t *testing.T) {
		got := parseIFSCBody([]byte{1, 2, 3})
		if got != nil {
			t.Errorf("parseIFSCBody of short slice = %+v, want nil", got)
		}
	})

	t.Run("parseIFSCBody length boundary", func(t *testing.T) {
		// 15 bytes can't hold the 16-byte fileID; 16 is the minimal valid body
		// (fileID present, zero slices).
		if got := parseIFSCBody(make([]byte, 15)); got != nil {
			t.Errorf("parseIFSCBody(15 bytes) = %+v, want nil", got)
		}
		got := parseIFSCBody(make([]byte, 16))
		if got == nil {
			t.Fatal("parseIFSCBody(16 bytes) = nil, want non-nil")
		}
		if len(got.slices) != 0 {
			t.Errorf("parseIFSCBody(16 bytes) slices = %d, want 0", len(got.slices))
		}
	})

	t.Run("parseFileDescBody length boundary", func(t *testing.T) {
		// 55 bytes can't hold FileID+FullHash+Hash16k+FileSize (56 bytes);
		// 56 is the minimal valid body (no filename); 57+ carries a filename.
		if got := parseFileDescBody(make([]byte, 55)); got != nil {
			t.Errorf("parseFileDescBody(55 bytes) = %+v, want nil", got)
		}

		minimal := parseFileDescBody(make([]byte, 56))
		if minimal == nil {
			t.Fatal("parseFileDescBody(56 bytes) = nil, want non-nil")
		}
		if minimal.FileName != "" {
			t.Errorf("parseFileDescBody(56 bytes) FileName = %q, want empty (no name bytes present)", minimal.FileName)
		}

		withName := make([]byte, 60)
		copy(withName[56:], "abcd")
		fd := parseFileDescBody(withName)
		if fd == nil {
			t.Fatal("parseFileDescBody(60 bytes) = nil, want non-nil")
		}
		if fd.FileName != "abcd" {
			t.Errorf("parseFileDescBody(60 bytes) FileName = %q, want %q", fd.FileName, "abcd")
		}
	})

	t.Run("parseIFSCBody valid", func(t *testing.T) {
		body := make([]byte, 16+20*2)
		fileID := [16]byte{1, 2, 3}
		copy(body[0:16], fileID[:])
		// Entry 1
		copy(body[16:32], []byte("md5hash1--------"))
		binary.LittleEndian.PutUint32(body[32:36], 0x11111111)
		// Entry 2
		copy(body[36:52], []byte("md5hash2--------"))
		binary.LittleEndian.PutUint32(body[52:56], 0x22222222)

		got := parseIFSCBody(body)
		if got == nil {
			t.Fatal("parseIFSCBody returned nil")
		}
		if got.fileID != fileID {
			t.Errorf("fileID = %v, want %v", got.fileID, fileID)
		}
		if len(got.slices) != 2 {
			t.Fatalf("len(slices) = %d, want 2", len(got.slices))
		}
		if got.slices[0].crc32 != 0x11111111 || got.slices[1].crc32 != 0x22222222 {
			t.Errorf("slices CRCs = [%x, %x], want [11111111, 22222222]", got.slices[0].crc32, got.slices[1].crc32)
		}
	})

	t.Run("reconstructCRCs", func(t *testing.T) {
		fileID := [16]byte{1}
		fd := &FileDesc{
			FileID:   fileID,
			FileSize: 5,
		}
		set := &ParsedSet{
			SliceSize: 3,
			FilesByID: map[[16]byte]*FileDesc{
				fileID: fd,
			},
		}

		// File content: "Hello"
		// Slice 0: "Hel"
		crc0 := crc32.ChecksumIEEE([]byte("Hel"))
		// Slice 1: "lo" padded with one zero to SliceSize (3) -> "lo\x00"
		crc1 := crc32.ChecksumIEEE([]byte("lo\x00"))

		ifscPackets := []ifscData{
			{
				fileID: fileID,
				slices: []ifscSlice{
					{crc32: crc0},
					{crc32: crc1},
				},
			},
		}

		reconstructCRCs(set, ifscPackets)

		wantCRC := crc32.ChecksumIEEE([]byte("Hello"))
		if fd.FileCRC32 != wantCRC {
			t.Errorf("reconstructCRCs combined CRC = %08x, want %08x", fd.FileCRC32, wantCRC)
		}
	})

	t.Run("buildBy16k with duplicates", func(t *testing.T) {
		h1 := [16]byte{1}
		h2 := [16]byte{2}

		set := &ParsedSet{
			Files: []FileDesc{
				{FileID: [16]byte{10}, Hash16k: h1},
				{FileID: [16]byte{20}, Hash16k: h1}, // duplicate of h1
				{FileID: [16]byte{30}, Hash16k: h2}, // unique
			},
			By16k: make(map[[16]byte]*FileDesc),
		}

		buildBy16k(set)

		if !set.Files[0].HasDuplicate {
			t.Error("expected Files[0] to be flagged as duplicate")
		}
		if !set.Files[1].HasDuplicate {
			t.Error("expected Files[1] to be flagged as duplicate")
		}
		if set.Files[2].HasDuplicate {
			t.Error("expected Files[2] to not be flagged as duplicate")
		}

		if _, ok := set.By16k[h1]; ok {
			t.Error("expected duplicate hash h1 to not be in By16k map")
		}
		fd, ok := set.By16k[h2]
		if !ok || fd.FileID != [16]byte{30} {
			t.Errorf("expected h2 in By16k map mapping to FileID 30, got ok=%v", ok)
		}
	})
}

func TestParsePar2Set_MaxPacketBodySize(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	packetLen := uint64(100 * 1024 * 1024) // 100 MiB
	buf := make([]byte, 64)
	copy(buf[0:8], magic)
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)

	f, err := os.Create(parPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		t.Fatalf("Write: %v", err)
	}
	// Truncate file so that packetLen <= fileSize check passes and we hit the body size check.
	if err := f.Truncate(int64(packetLen)); err != nil {
		f.Close()
		t.Fatalf("Truncate: %v", err)
	}
	f.Close()

	_, err = ParsePar2Set(parPath)
	if err == nil {
		t.Fatal("ParsePar2Set expected error when packet body size exceeds 64 MiB, got nil")
	}
}

func TestParsePar2Set_MaxPacketBodySizeBoundary(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	// Exactly 64 MiB body (packetLen = 64 MiB + 64 bytes header).
	packetLen := defaultMaxPacketBodySize + 64
	buf := make([]byte, 64)
	copy(buf[0:8], magic)
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)

	f, err := os.Create(parPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		t.Fatalf("Write: %v", err)
	}
	if err := f.Truncate(int64(packetLen)); err != nil {
		f.Close()
		t.Fatalf("Truncate: %v", err)
	}
	f.Close()

	_, err = ParsePar2Set(parPath)
	if err != nil {
		t.Fatalf("ParsePar2Set: expected exactly 64 MiB packet body to pass size check, got error: %v", err)
	}
}

func TestParsePar2Set_PacketBodySizeExceeded(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test_large_body.par2")

	// Packet body size 70000 bytes (> MinPar2PacketBodySize 65536)
	bodyLen := uint64(70000)
	packetLen := 64 + bodyLen
	buf := make([]byte, 64)
	copy(buf[0:8], magic)
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)

	f, err := os.Create(parPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		t.Fatalf("Write: %v", err)
	}
	if err := f.Truncate(int64(packetLen)); err != nil {
		f.Close()
		t.Fatalf("Truncate: %v", err)
	}
	f.Close()

	opts := ParseOptions{
		MaxPacketBodySize: 65536,
		MaxJunkScanBytes:  65536,
	}

	_, err = ParsePar2SetWithOptions(parPath, opts)
	if err == nil {
		t.Fatalf("expected error when bodyLen (70000) > MaxPacketBodySize (65536), got nil")
	}

	if !errors.Is(err, ErrPacketBodySizeExceeded) {
		t.Errorf("expected errors.Is(err, ErrPacketBodySizeExceeded), got: %v", err)
	}

	wantSubstring := "par2 packet body size (70000 bytes) exceeds configured limit (65536 bytes); increase 'par2_max_packet_body_size' in Settings -> Post-Processing if this is a valid ultra-large file"
	if !bytes.Contains([]byte(err.Error()), []byte(wantSubstring)) {
		t.Errorf("error string %q does not contain expected substring %q", err.Error(), wantSubstring)
	}
}

func TestParsePar2Set_JunkScanLimitExceeded(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test_junk.par2")

	// Write 2000 bytes of junk without magic header
	junk := make([]byte, 2000)
	for i := range junk {
		junk[i] = 0xAA
	}

	if err := os.WriteFile(parPath, junk, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	opts := ParseOptions{
		MaxJunkScanBytes:  1024,
		MaxPacketBodySize: 67108864,
	}

	_, err := ParsePar2SetWithOptions(parPath, opts)
	if err == nil {
		t.Fatalf("expected error when junk scan exceeds limit, got nil")
	}

	if !errors.Is(err, ErrJunkScanLimitExceeded) {
		t.Errorf("expected errors.Is(err, ErrJunkScanLimitExceeded), got: %v", err)
	}

	wantSubstring := "par2 junk scan exceeded limit (1024 bytes) without finding header magic; increase 'par2_max_junk_scan_bytes' in Settings -> Post-Processing if file has a large header"
	if !bytes.Contains([]byte(err.Error()), []byte(wantSubstring)) {
		t.Errorf("error string %q does not contain expected substring %q", err.Error(), wantSubstring)
	}
}

func TestParsePar2Set_MinimumSafetyFloors(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test_floor.par2")

	setID := [16]byte{0x01}
	fileID := [16]byte{0x02}
	mainPkt := buildPacket(setID, typeMain, buildMainBody(16384, fileID))

	if err := os.WriteFile(parPath, mainPkt, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Below safety floor values: MaxPacketBodySize=10 (< MinPar2PacketBodySize 65536), MaxJunkScanBytes=50 (< MinPar2JunkScanBytes 1024)
	// Should trigger fallback to defaults (64 MiB packet body, 64 KiB junk scan), allowing the 24-byte mainPkt body to parse successfully.
	opts := ParseOptions{
		MaxPacketBodySize: 10,
		MaxJunkScanBytes:  50,
	}

	set, err := ParsePar2SetWithOptions(parPath, opts)
	if err != nil {
		t.Fatalf("expected ParsePar2SetWithOptions to succeed with safety floor fallback, got: %v", err)
	}

	if set.SetID != setID {
		t.Errorf("SetID = %x, want %x", set.SetID, setID)
	}
}

func TestParseOptionsFromConfig(t *testing.T) {
	// Nil config fallback
	optsNil := ParseOptionsFromConfig(nil)
	defOpts := DefaultParseOptions()
	if optsNil != defOpts {
		t.Errorf("ParseOptionsFromConfig(nil) = %+v, want %+v", optsNil, defOpts)
	}

	// Non-nil config mapping
	cfg := &config.PostProcConfig{
		Par2MaxJunkScanBytes:  2048,
		Par2MaxPacketBodySize: 131072,
	}
	opts := ParseOptionsFromConfig(cfg)
	if opts.MaxJunkScanBytes != 2048 {
		t.Errorf("MaxJunkScanBytes = %d, want 2048", opts.MaxJunkScanBytes)
	}
	if opts.MaxPacketBodySize != 131072 {
		t.Errorf("MaxPacketBodySize = %d, want 131072", opts.MaxPacketBodySize)
	}
}
