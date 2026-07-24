package postproc

import (
	"crypto/md5"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writePar2 creates a minimal PAR2 file in dir mapping fileData's 16K-MD5 to fileName.
func writePar2(t *testing.T, dir, fileName string, fileData []byte) {
	t.Helper()
	h := md5.New() //nolint:gosec // MD5 is PAR2 spec; not used for security
	h.Write(fileData[:min(16384, len(fileData))])
	var hash16k [16]byte
	copy(hash16k[:], h.Sum(nil))

	fileNameBytes := []byte(fileName)
	if pad := (4 - len(fileNameBytes)%4) % 4; pad > 0 {
		fileNameBytes = append(fileNameBytes, make([]byte, pad)...)
	}
	bodyLen := uint64(16 + 16 + 16 + 8 + len(fileNameBytes))
	packetLen := 64 + bodyLen
	buf := make([]byte, packetLen)
	copy(buf[0:8], []byte("PAR2\x00PKT"))
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)
	copy(buf[48:64], []byte{'P', 'A', 'R', ' ', '2', '.', '0', '\x00', 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'})
	copy(buf[64+16+16:64+32+16], hash16k[:])
	binary.LittleEndian.PutUint64(buf[64+48:64+56], uint64(len(fileData)))
	copy(buf[64+56:], fileNameBytes)
	ph := md5.New() //nolint:gosec
	ph.Write(buf[32:64])
	ph.Write(buf[64:])
	copy(buf[16:32], ph.Sum(nil))

	parPath := filepath.Join(dir, "test.par2")
	if err := os.WriteFile(parPath, buf, 0644); err != nil {
		t.Fatalf("WriteFile PAR2: %v", err)
	}
}

func TestRecoverPar2NamesStage_Run(t *testing.T) {
	t.Parallel()

	stage := NewRecoverPar2NamesStage()
	if stage.Name() != "recover_par2_names" {
		t.Errorf("Name() = %q; want recover_par2_names", stage.Name())
	}

	// 1. Test clean run (no renames)
	t.Run("no renames", func(t *testing.T) {
		job := &Job{
			Queue:       newQueueJob(t, "testjob", 0),
			DownloadDir: t.TempDir(),
		}
		if err := stage.Run(t.Context(), job); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	// 2. Test successful par2 rename path
	t.Run("successful rename", func(t *testing.T) {
		jobDir := t.TempDir()

		fileName := "original.mkv"
		fileData := []byte("this is more than 16kb of data " + string(make([]byte, 20000)))

		obfPath := filepath.Join(jobDir, "abcdef1234567890.mkv")
		if err := os.WriteFile(obfPath, fileData, 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		writePar2(t, jobDir, fileName, fileData)

		job := &Job{
			Queue:       newQueueJob(t, "testjob", 0),
			DownloadDir: jobDir,
		}

		if err := stage.Run(t.Context(), job); err != nil {
			t.Fatalf("Run: %v", err)
		}

		// Verify file was actually renamed.
		wantTo := filepath.Join(jobDir, fileName)
		if _, err := os.Stat(wantTo); err != nil {
			t.Errorf("Stat %q: %v", wantTo, err)
		}
	})

	// 3. Test error path
	t.Run("error path", func(t *testing.T) {
		job := &Job{
			Queue:       newQueueJob(t, "testjob", 0),
			DownloadDir: "/nonexistent-path-abc-123",
		}
		// Should log warning but not fail Run.
		if err := stage.Run(t.Context(), job); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
}
