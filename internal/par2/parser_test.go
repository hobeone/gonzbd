package par2

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseFileDescriptions(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	// Create a minimal synthetic PAR2 file with a File Description packet.
	// Header (64 bytes) + Body (FileID 16 + HashFull 16 + Hash16k 16 + FileLength 8 + FileName var)
	fileName := "original.mkv"
	fileNameBytes := []byte(fileName)
	// Pad fileName to 4-byte boundary.
	padding := (4 - (len(fileNameBytes) % 4)) % 4
	fileNameBytes = append(fileNameBytes, make([]byte, padding)...)

	bodyLen := uint64(16 + 16 + 16 + 8 + len(fileNameBytes))
	packetLen := 64 + bodyLen

	buf := make([]byte, packetLen)
	// Header
	copy(buf[0:8], magic)
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)
	// Skip MD5 (16 bytes) and Recovery Set ID (16 bytes)
	copy(buf[48:64], typeFileDesc[:])

	// Body
	// Skip FileID (16 bytes) and HashFull (16 bytes)
	hash16k := [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	copy(buf[64+16+16:64+16+16+16], hash16k[:])
	binary.LittleEndian.PutUint64(buf[64+16+16+16:64+16+16+16+8], 123456)
	copy(buf[64+56:], fileNameBytes)

	if err := os.WriteFile(parPath, buf, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ParseFileDescriptions(parPath)
	if err != nil {
		t.Fatalf("ParseFileDescriptions: %v", err)
	}

	want := []FileDesc{
		{
			FileName: fileName,
			Hash16k:  hash16k,
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseFileDescriptions = %v; want %v", got, want)
	}
}

func TestParseFileDescriptions_MalformedLength(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	packetLen := uint64(1024 * 1024 * 1024) // 1GB
	buf := make([]byte, 64)
	copy(buf[0:8], magic)
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)

	if err := os.WriteFile(parPath, buf, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ParseFileDescriptions(parPath)
	if err == nil {
		t.Fatal("ParseFileDescriptions expected error on massive packetLen, got nil")
	}
}
