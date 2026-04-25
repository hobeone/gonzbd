package deobfuscate

import (
	"crypto/md5"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestPar2Rename(t *testing.T) {
	tmpDir := t.TempDir()
	jobDir := filepath.Join(tmpDir, "job_folder")
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// 1. Create an obfuscated file.
	fileName := "original.mkv"
	fileData := []byte("this is more than 16kb of data " + string(make([]byte, 20000)))
	h := md5.New()
	h.Write(fileData[:16384])
	hash16k := [16]byte{}
	copy(hash16k[:], h.Sum(nil))

	obfPath := filepath.Join(jobDir, "abcdef1234567890.mkv")
	if err := os.WriteFile(obfPath, fileData, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 2. Create a PAR2 file that maps the hash to the original filename.
	parPath := filepath.Join(jobDir, "test.par2")

	fileNameBytes := []byte(fileName)
	padding := (4 - (len(fileNameBytes) % 4)) % 4
	fileNameBytes = append(fileNameBytes, make([]byte, padding)...)

	bodyLen := uint64(16 + 16 + 16 + 8 + len(fileNameBytes))
	packetLen := 64 + bodyLen

	buf := make([]byte, packetLen)
	copy(buf[0:8], []byte("PAR2\x00\x00\x00\x00"))
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)
	copy(buf[48:64], []byte{'P', 'A', 'R', ' ', '2', '.', '0', '\x00', 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'})

	copy(buf[64+16+16:64+16+16+16], hash16k[:])
	binary.LittleEndian.PutUint64(buf[64+16+16+16:64+16+16+16+8], uint64(len(fileData)))
	copy(buf[64+56:], fileNameBytes)

	if err := os.WriteFile(parPath, buf, 0644); err != nil {
		t.Fatalf("WriteFile PAR2: %v", err)
	}

	// 3. Run Par2Rename.
	renames, err := Par2Rename(jobDir)
	if err != nil {
		t.Fatalf("Par2Rename: %v", err)
	}

	if len(renames) != 1 {
		t.Fatalf("len(renames) = %d; want 1", len(renames))
	}

	if renames[0].From != obfPath {
		t.Errorf("rename.From = %q; want %q", renames[0].From, obfPath)
	}

	wantTo := filepath.Join(jobDir, fileName)
	if renames[0].To != wantTo {
		t.Errorf("rename.To = %q; want %q", renames[0].To, wantTo)
	}

	// Verify file was actually renamed.
	if _, err := os.Stat(wantTo); err != nil {
		t.Errorf("Stat %q: %v", wantTo, err)
	}
	if _, err := os.Stat(obfPath); !os.IsNotExist(err) {
		t.Errorf("Obf file %q still exists", obfPath)
	}
}

