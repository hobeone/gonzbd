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
	renames, err := testPar2Rename(context.Background(), slog.Default(), jobDir, fsutil.SanitizeOptions{})
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

	renames, err := testPar2Rename(context.Background(), slog.Default(), jobDir, fsutil.SanitizeOptions{})
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

	renames, err := testPar2Rename(context.Background(), slog.Default(), jobDir, fsutil.SanitizeOptions{})
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
	renames, err := testPar2Rename(context.Background(), nil, jobDir, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("Par2Rename with nil logger: %v", err)
	}
	if len(renames) != 0 {
		t.Errorf("expected 0 renames, got %d", len(renames))
	}
}

func TestDeobfuscate_Par2RenameError(t *testing.T) {
	jobDir := t.TempDir()

	// Create a file and a par2 file mapping to it
	fileName := "original.mkv"
	fileData := make([]byte, 11*1024*1024)
	copy(fileData, []byte("this is more than 10MB of data"))

	obfPath := filepath.Join(jobDir, "abcdef1234567890.mkv")
	if err := os.WriteFile(obfPath, fileData, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writePar2(t, jobDir, fileName, fileData)

	// Make the directory read-only (no write permissions)
	if err := os.Chmod(jobDir, 0555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer os.Chmod(jobDir, 0755) // restore for cleanup

	handler := &recordHandler{}
	log := slog.New(handler)

	_, err := Deobfuscate(context.Background(), log, jobDir, "MovieName", fsutil.SanitizeOptions{})
	if err == nil {
		t.Error("expected Deobfuscate to fail when directory is read-only")
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

func TestPar2Rename_InvalidPar2File(t *testing.T) {
	tmpDir := t.TempDir()
	jobDir := filepath.Join(tmpDir, "job_folder")
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Create an obfuscated file
	fileData := []byte("this is more than 16kb of data " + string(make([]byte, 20000)))
	obfPath := filepath.Join(jobDir, "abcdef1234567890.mkv")
	if err := os.WriteFile(obfPath, fileData, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create an invalid par2 file that triggers a parsing error (invalid packet length)
	invalidPar2Path := filepath.Join(jobDir, "invalid.par2")
	invalidContent := make([]byte, 64)
	copy(invalidContent[0:8], "PAR2\x00PKT\x00")
	// Set packet length (bytes 8-16) to 1 (which is < 64)
	invalidContent[8] = 1
	if err := os.WriteFile(invalidPar2Path, invalidContent, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Run Par2Rename. It should succeed but not rename anything because the par2 is invalid.
	renames, err := testPar2Rename(context.Background(), slog.Default(), jobDir, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("Par2Rename: %v", err)
	}
	if len(renames) != 0 {
		t.Errorf("len(renames) = %d; want 0", len(renames))
	}
}

func testPar2Rename(ctx context.Context, log *slog.Logger, dir string, opts fsutil.SanitizeOptions) ([]Rename, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return Par2Rename(ctx, log, root, dir, opts)
}

func TestDeobfuscateHelpersAndExcluding(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)

	fileData := make([]byte, 20000)
	copy(fileData, []byte("payload for par2 exclusion test"))
	obfPath := filepath.Join(dir, "abcdef1234567890.mkv")
	if err := os.WriteFile(obfPath, fileData, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sibPath := filepath.Join(dir, "abcdef1234567890.nfo")
	if err := os.WriteFile(sibPath, []byte("nfo"), 0o600); err != nil {
		t.Fatalf("WriteFile sib: %v", err)
	}
	writePar2(t, dir, "original.mkv", fileData)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	par2s := findPar2Files(entries, dir)
	if len(par2s) != 1 {
		t.Fatalf("findPar2Files = %v, want 1 par2 file", par2s)
	}
	hashes := buildHashToNameMap(log, par2s)
	if len(hashes) != 1 {
		t.Fatalf("buildHashToNameMap = %v, want 1 entry", hashes)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// 1. Par2RenameExcluding: excluding test.par2 or the target file prevents renaming.
	renames, err := Par2RenameExcluding(t.Context(), log, root, dir, fsutil.SanitizeOptions{}, map[string]struct{}{"test.par2": {}})
	if err != nil {
		t.Fatalf("Par2RenameExcluding(exclude par2): %v", err)
	}
	if len(renames) != 0 {
		t.Fatalf("expected 0 renames when par2 file is excluded, got %v", renames)
	}
	renames, err = Par2RenameExcluding(t.Context(), log, root, dir, fsutil.SanitizeOptions{}, map[string]struct{}{"abcdef1234567890.mkv": {}})
	if err != nil {
		t.Fatalf("Par2RenameExcluding(exclude candidate): %v", err)
	}
	if len(renames) != 0 {
		t.Fatalf("expected 0 renames when candidate is excluded, got %v", renames)
	}

	// 2. extractRARUsefulName: excluding the RAR archive skips reading its header.
	rarBytes, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "rar", "sample.rar"))
	if err != nil {
		t.Fatalf("ReadFile sample.rar: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "archive.rar"), rarBytes, 0o600); err != nil {
		t.Fatalf("WriteFile archive.rar: %v", err)
	}
	if got := extractRARUsefulName(root, dir, log, nil); got != "sample" {
		t.Errorf("extractRARUsefulName without exclusion = %q, want sample", got)
	}
	if got := extractRARUsefulName(root, dir, log, map[string]struct{}{"archive.rar": {}}); got != "" {
		t.Errorf("extractRARUsefulName with archive.rar excluded = %q, want empty", got)
	}

	// Excluding Phase 1 (par2-based rename returns early when unexcluded).
	parPhaseDir := t.TempDir()
	writePar2(t, parPhaseDir, "original.mkv", fileData)
	if err := os.WriteFile(filepath.Join(parPhaseDir, "abcdef1234567890.mkv"), fileData, 0o600); err != nil {
		t.Fatalf("WriteFile parPhaseDir/abcdef1234567890.mkv: %v", err)
	}
	parPhaseRenames, err := Excluding(t.Context(), log, parPhaseDir, "Fallback", fsutil.SanitizeOptions{}, nil)
	if err != nil || len(parPhaseRenames) != 1 || filepath.Base(parPhaseRenames[0].To) != "original.mkv" {
		t.Fatalf("Excluding Phase 1 = %v, %v; want [original.mkv]", parPhaseRenames, err)
	}

	// 3. Excluding + SubtitlesExcluding: excluding a competing 10 MiB file and an .srt file.
	subDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(subDir, "archive.rar"), rarBytes, 0o600); err != nil {
		t.Fatalf("WriteFile subDir/archive.rar: %v", err)
	}
	bigMkv := filepath.Join(subDir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv")
	if err := os.WriteFile(bigMkv, make([]byte, 11*1024*1024), 0o600); err != nil {
		t.Fatalf("WriteFile bigMkv: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "competing.bin"), make([]byte, 10*1024*1024), 0o600); err != nil {
		t.Fatalf("WriteFile competing.bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "keep.srt"), []byte("srt1"), 0o600); err != nil {
		t.Fatalf("WriteFile keep.srt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "excluded.srt"), []byte("srt2"), 0o600); err != nil {
		t.Fatalf("WriteFile excluded.srt: %v", err)
	}
	// Without excluding competing.bin, no 3x dominant file exists for SubtitlesExcluding.
	if noDom, sErr := SubtitlesExcluding(log, subDir, nil); sErr != nil || len(noDom) != 0 {
		t.Fatalf("SubtitlesExcluding without exclusion = %v, %v; want empty", noDom, sErr)
	}
	excl := map[string]struct{}{"archive.rar": {}, "competing.bin": {}, "excluded.srt": {}}
	deobRenames, err := Excluding(t.Context(), log, subDir, "Clean.Title", fsutil.SanitizeOptions{}, excl)
	if err != nil {
		t.Fatalf("Excluding: %v", err)
	}
	if len(deobRenames) != 1 || filepath.Base(deobRenames[0].To) != "Clean.Title.mkv" {
		t.Fatalf("Excluding renames = %v, want [Clean.Title.mkv]", deobRenames)
	}
	srtRenames, err := SubtitlesExcluding(log, subDir, excl)
	if err != nil {
		t.Fatalf("SubtitlesExcluding: %v", err)
	}
	if len(srtRenames) != 1 || filepath.Base(srtRenames[0].To) != "Clean.Title.keep.srt" {
		t.Fatalf("SubtitlesExcluding renames = %v, want [Clean.Title.keep.srt]", srtRenames)
	}
	if _, err := Excluding(t.Context(), log, filepath.Join(subDir, "missing"), "Title", fsutil.SanitizeOptions{}, nil); err == nil {
		t.Error("expected error from Excluding on missing directory")
	}
	if _, err := SubtitlesExcluding(log, filepath.Join(subDir, "missing"), nil); err == nil {
		t.Error("expected error from SubtitlesExcluding on missing directory")
	}

	if !hasObfuscatedPattern(log, "b082fa0beaa644d3aa01045d5b8d0b36") {
		t.Error("hasObfuscatedPattern(32-hex) = false, want true")
	}
	if got := originalStem("/tmp/foo.junk.mkv", []Rename{{From: "/tmp/foo.junk", To: "/tmp/foo.junk.mkv"}}); got != "/tmp/foo" {
		t.Errorf("originalStem = %q, want /tmp/foo", got)
	}

	sibRenames, err := renameSiblings(
		log,
		root,
		dir,
		"Clean.Movie",
		obfPath,
		[]string{obfPath, sibPath},
		[]string{"abcdef1234567890.mkv", "abcdef1234567890.nfo"},
		nil,
		fsutil.SanitizeOptions{},
	)
	if err != nil {
		t.Fatalf("renameSiblings: %v", err)
	}
	if len(sibRenames) != 1 {
		t.Fatalf("renameSiblings = %v, want 1 sibling rename", sibRenames)
	}
	if _, err := renameRecorded(log, root, "Clean.Movie.nfo", "abcdef1234567890.nfo", sibRenames[0].To, sibPath, "", "restore"); err != nil {
		t.Fatalf("renameRecorded: %v", err)
	}

	dedupDir := t.TempDir()
	dedupRoot, err := os.OpenRoot(dedupDir)
	if err != nil {
		t.Fatalf("OpenRoot dedupDir: %v", err)
	}
	defer dedupRoot.Close()
	for name, data := range map[string][]byte{
		"Clean.Movie.mkv":                      []byte("same-bytes"),
		"b082fa0beaa644d3aa01045d5b8d0b36.mkv": []byte("same-bytes"),
		"0675e29e9abfd2f7d069dab0b853283c.mkv": []byte("diff-bytes"),
		"Notes.File.mkv":                       []byte("same-bytes"),
	} {
		if err := os.WriteFile(filepath.Join(dedupDir, name), data, 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	inRels := []string{
		"Clean.Movie.mkv",
		"b082fa0beaa644d3aa01045d5b8d0b36.mkv",
		"0675e29e9abfd2f7d069dab0b853283c.mkv",
		"Notes.File.mkv",
	}
	inPaths := make([]string, len(inRels))
	for i, r := range inRels {
		inPaths[i] = filepath.Join(dedupDir, r)
	}
	_, outRels := dedupObfuscatedAgainstTarget(log, dedupRoot, "Clean.Movie", inPaths, inRels, fsutil.SanitizeOptions{})
	if len(outRels) != 3 {
		t.Fatalf("dedupObfuscatedAgainstTarget = %v, want 3 surviving entries", outRels)
	}
	if _, err := os.Stat(filepath.Join(dedupDir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv")); !os.IsNotExist(err) {
		t.Errorf("identical obfuscated file still exists: err=%v", err)
	}
}
