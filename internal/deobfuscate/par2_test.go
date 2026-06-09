package deobfuscate

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
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

func TestPar2Rename(t *testing.T) {
	tmpDir := t.TempDir()
	jobDir := filepath.Join(tmpDir, "job_folder")
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// 1. Create an obfuscated file.
	fileName := "original.mkv"
	fileData := []byte("this is more than 16kb of data " + string(make([]byte, 20000)))

	obfPath := filepath.Join(jobDir, "abcdef1234567890.mkv")
	if err := os.WriteFile(obfPath, fileData, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 2. Create a PAR2 file that maps the hash to the original filename.
	writePar2(t, jobDir, fileName, fileData)

	// 3. Run Par2Rename.
	renames, err := Par2Rename(context.Background(), slog.Default(), jobDir, fsutil.SanitizeOptions{})
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

// TestPar2Rename_CollisionIdentical: target already exists with the same
// content. The obfuscated source should be deleted; no rename is returned.
func TestPar2Rename_CollisionIdentical(t *testing.T) {
	jobDir := t.TempDir()

	content := []byte("small nfo file, well under 16 kB")
	trueName := "Stratovarius-Infinite.nfo"

	obfPath := filepath.Join(jobDir, "000-obfuscated.nfo")
	if err := os.WriteFile(obfPath, content, 0644); err != nil {
		t.Fatalf("WriteFile obf: %v", err)
	}

	// Pre-existing file with the same content at the true name.
	existingPath := filepath.Join(jobDir, trueName)
	if err := os.WriteFile(existingPath, content, 0644); err != nil {
		t.Fatalf("WriteFile existing: %v", err)
	}

	writePar2(t, jobDir, trueName, content)

	renames, err := Par2Rename(context.Background(), slog.Default(), jobDir, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("Par2Rename: %v", err)
	}
	if len(renames) != 0 {
		t.Errorf("renames = %d; want 0 (duplicate should be deleted, not renamed)", len(renames))
	}
	// Obfuscated file must be gone.
	if _, err := os.Stat(obfPath); !os.IsNotExist(err) {
		t.Errorf("obfuscated file %q still exists after identical-content deletion", obfPath)
	}
	// True-name file must still exist.
	if _, err := os.Stat(existingPath); err != nil {
		t.Errorf("existing true-name file %q was unexpectedly removed: %v", existingPath, err)
	}
}

// TestPar2Rename_CollisionDifferent: target already exists but with different
// content. The obfuscated source should be renamed to a .1 variant.
func TestPar2Rename_CollisionDifferent(t *testing.T) {
	jobDir := t.TempDir()

	obfContent := []byte("this is the obfuscated file's content")
	existingContent := []byte("this is a different file that happens to share the name")
	trueName := "Stratovarius-Infinite.nfo"

	obfPath := filepath.Join(jobDir, "000-obfuscated.nfo")
	if err := os.WriteFile(obfPath, obfContent, 0644); err != nil {
		t.Fatalf("WriteFile obf: %v", err)
	}

	existingPath := filepath.Join(jobDir, trueName)
	if err := os.WriteFile(existingPath, existingContent, 0644); err != nil {
		t.Fatalf("WriteFile existing: %v", err)
	}

	writePar2(t, jobDir, trueName, obfContent)

	renames, err := Par2Rename(context.Background(), slog.Default(), jobDir, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("Par2Rename: %v", err)
	}
	if len(renames) != 1 {
		t.Fatalf("renames = %d; want 1", len(renames))
	}

	wantTo := filepath.Join(jobDir, "Stratovarius-Infinite.1.nfo")
	if renames[0].To != wantTo {
		t.Errorf("rename.To = %q; want %q", renames[0].To, wantTo)
	}
	// Both files must exist: the pre-existing one at the true name and the
	// renamed obfuscated one at the .1 variant.
	if _, err := os.Stat(existingPath); err != nil {
		t.Errorf("existing file %q missing: %v", existingPath, err)
	}
	if _, err := os.Stat(wantTo); err != nil {
		t.Errorf(".1 renamed file %q missing: %v", wantTo, err)
	}
}

type recordHandler struct {
	records []slog.Record
}

func (h *recordHandler) Enabled(ctx context.Context, l slog.Level) bool { return true }
func (h *recordHandler) Handle(ctx context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *recordHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(name string) slog.Handler       { return h }

func TestPar2Rename_NilLogger(t *testing.T) {
	t.Parallel()
	jobDir := t.TempDir()
	renames, err := Par2Rename(context.Background(), nil, jobDir, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("Par2Rename with nil logger: %v", err)
	}
	if len(renames) != 0 {
		t.Errorf("expected 0 renames, got %d", len(renames))
	}
}

func TestDeobfuscate_Par2RenameError(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "not_a_dir.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	handler := &recordHandler{}
	log := slog.New(handler)

	_, err := Deobfuscate(context.Background(), log, filePath, "MovieName", fsutil.SanitizeOptions{})
	if err == nil {
		t.Error("expected Deobfuscate to fail when path is not a directory")
	}

	foundWarn := false
	for _, rec := range handler.records {
		if rec.Level == slog.LevelWarn && strings.Contains(rec.Message, "par2 deobfuscation encountered an error") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Error("expected a warning log to be recorded when Par2Rename failed")
	}
}
