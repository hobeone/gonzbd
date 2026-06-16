package postproc

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// unpackFixture returns the absolute path to a file in the shared
// unpack testdata directory. Go tests run with cwd = package directory,
// so "../unpack/testdata" is stable and trimpath-safe.
func unpackFixture(name string) string {
	return filepath.Join("..", "unpack", "testdata", name)
}

// copyToDir copies src to dstDir/baseName and returns the destination path.
func copyToDir(t *testing.T, src, dstDir string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	dst := filepath.Join(dstDir, filepath.Base(src))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

// enabledUnpackStage builds an UnpackStage that is enabled and uses
// pure-Go RAR extraction so tests run without an external unrar binary.
func enabledUnpackStage() *UnpackStage {
	s := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, false)
	s.SetEnabled(true)
	return s
}

// ---------- Extraction path tests ----------

// TestUnpackStage_ExtractSingleRarGoMode verifies the GoRAR extraction path:
// a healthy single-volume RAR is extracted and the job has no UnpackError.
func TestUnpackStage_ExtractSingleRarGoMode(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	if err := enabledUnpackStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.UnpackError {
		t.Error("UnpackError = true; want false")
	}
	// At least the three files from single_rar5.rar must be present after extraction.
	for _, name := range []string{"file1.txt", "file2.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("extracted file %s not found: %v", name, err)
		}
	}
}

// TestUnpackStage_CleanupDeletesArchiveParts verifies that when cleanup is
// enabled, the source RAR file is deleted after successful extraction.
func TestUnpackStage_CleanupDeletesArchiveParts(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	rarPath := copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	s := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true /* cleanup */)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(rarPath); !os.IsNotExist(err) {
		t.Errorf("archive still exists after cleanup: want removed, got %v", err)
	}
}

// TestUnpackStage_CorruptRarGoMode verifies that extracting a corrupt RAR
// sets job.UnpackError and returns an error.
func TestUnpackStage_CorruptRarGoMode(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("corrupt.rar"), dir)

	if err := enabledUnpackStage().Run(t.Context(), job); err == nil {
		t.Fatal("expected error for corrupt RAR, got nil")
	}
	if !job.UnpackError {
		t.Error("UnpackError = false; want true for corrupt RAR")
	}
}

// TestUnpackStage_DirectUnpackPrePopulated verifies that RAR parts from
// DirectUnpackSets are included in allSuccessful so cleanup can remove them.
func TestUnpackStage_DirectUnpackPrePopulated(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)

	// Create a fake RAR part on disk (content doesn't matter; cleanup just os.Remove it).
	fakePart := filepath.Join(dir, "movie.part01.rar")
	if err := os.WriteFile(fakePart, []byte("x"), 0o644); err != nil {
		t.Fatalf("write fake part: %v", err)
	}

	job.DirectUnpackSets = map[string]directunpack.SuccessSet{
		"movie": {
			RarParts:       []string{fakePart},
			ExtractedFiles: []string{filepath.Join(dir, "movie.mkv")},
		},
	}

	s := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true /* cleanup */)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Cleanup must have deleted the fake part that DirectUnpack recorded.
	if _, err := os.Stat(fakePart); !os.IsNotExist(err) {
		t.Errorf("DirectUnpack part %s still on disk after cleanup", fakePart)
	}
}

// TestUnpackStage_DisabledSkips verifies that a disabled stage returns nil
// immediately and sets no error flags on the job.
func TestUnpackStage_DisabledSkips(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	s := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, false)
	// deliberately NOT calling s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.UnpackError {
		t.Error("UnpackError set on disabled stage")
	}
	// The archive must still be present (stage did nothing).
	if _, err := os.Stat(filepath.Join(dir, "single_rar5.rar")); err != nil {
		t.Errorf("archive removed even though stage is disabled: %v", err)
	}
}

// ---------- applyPermissions helper ----------

// TestApplyPermissions_SetsFileModeOnFiles verifies that applyPermissions
// applies the given mode to files (execute bits stripped) and directories
// (full mode).
func TestApplyPermissions_SetsFileModeOnFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	count, err := applyPermissions(dir, "755")
	if err != nil {
		t.Fatalf("applyPermissions: %v", err)
	}
	if count == 0 {
		t.Error("count = 0; want > 0")
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	// Regular files must have execute bits stripped: 0755 → 0644.
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode = %04o; want 0644", got)
	}

	di, err := os.Stat(subDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o755 {
		t.Errorf("dir mode = %04o; want 0755", got)
	}
}

// TestApplyPermissions_BadPermString verifies that an invalid octal string
// returns an error.
func TestApplyPermissions_BadPermString(t *testing.T) {
	t.Parallel()

	_, err := applyPermissions(t.TempDir(), "not-octal")
	if err == nil {
		t.Error("expected error for bad perm string, got nil")
	}
}

// ---------- archiveTypePriority helper ----------

// TestArchiveTypePriority_SplitFirst verifies the sort invariant:
// SplitArchive < RarArchive < SevenZipArchive.
// SABnzbd requires file-join to run before RAR extraction in the same pass
// so that joined output can be immediately extracted.
func TestArchiveTypePriority_SplitFirst(t *testing.T) {
	t.Parallel()

	if archiveTypePriority(unpack.SplitArchive) >= archiveTypePriority(unpack.RarArchive) {
		t.Error("SplitArchive priority must be less than RarArchive")
	}
	if archiveTypePriority(unpack.RarArchive) >= archiveTypePriority(unpack.SevenZipArchive) {
		t.Error("RarArchive priority must be less than SevenZipArchive")
	}
}

// TestArchiveTypeName_KnownTypes verifies that each archive type maps to the
// correct tool name used in stage log output.
func TestArchiveTypeName_KnownTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		at   unpack.ArchiveType
		want string
	}{
		{unpack.RarArchive, "unrar"},
		{unpack.SevenZipArchive, "7zip"},
		{unpack.SplitArchive, "filejoin"},
		{unpack.UnknownArchive, "unpack"},
	}
	for _, tc := range cases {
		if got := archiveTypeName(tc.at); got != tc.want {
			t.Errorf("archiveTypeName(%v) = %q, want %q", tc.at, got, tc.want)
		}
	}
}

func TestUnpackHelpers(t *testing.T) {
	t.Parallel()

	// Direct references for alignment check
	_ = extractRARArchive
	_ = extractSevenZipArchive
	_ = (*UnpackStage).extractPendingArchives

	// 1. Test prepareOptions
	t.Run("prepareOptions", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue: &queue.Job{
				Password: "jobpass",
			},
		}

		// Create a temporary password file
		tmpFile := filepath.Join(t.TempDir(), "passwords.txt")
		content := "pass1\npass2\n"
		if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		opts := u.prepareOptions(t.Context(), slog.Default(), job, unpack.Options{}, tmpFile)
		if len(opts.Passwords) != 3 {
			t.Errorf("len(Passwords) = %d; want 3", len(opts.Passwords))
		}
		if opts.Passwords[0] != "jobpass" {
			t.Errorf("Passwords[0] = %q; want jobpass", opts.Passwords[0])
		}

		// Non-existent password file
		_ = u.prepareOptions(t.Context(), slog.Default(), job, unpack.Options{}, "nonexistent-password-file.txt")
	})

	// 2. Test applyPermissions
	t.Run("applyPermissions", func(t *testing.T) {
		u := NewUnpackStage()
		dir := t.TempDir()
		// create a file inside dir
		fPath := filepath.Join(dir, "file.txt")
		if err := os.WriteFile(fPath, []byte("hello"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: dir,
		}
		u.applyPermissions(t.Context(), slog.Default(), job, "755", []unpack.Archive{{}})
	})

	// 3. Test extractArchive RAR scenarios
	t.Run("extractArchive RAR", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		a := unpack.Archive{Type: unpack.RarArchive, Name: "nonexistent.rar"}

		// Scenario A: Native Go, no fallback
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGoRAR: true, GoRarFallback: false}, true)

		// Scenario B: External only
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGoRAR: false, GoRarFallback: false}, true)

		// Scenario C: Native Go with external fallback
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGoRAR: true, GoRarFallback: true}, true)
	})

	// 4. Test extractArchive 7-Zip scenarios
	t.Run("extractArchive 7-Zip", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		a := unpack.Archive{Type: unpack.SevenZipArchive, Name: "nonexistent.7z"}

		// Scenario A: Native Go, no fallback
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGo7z: true, Go7zFallback: false}, true)

		// Scenario B: External only
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGo7z: false, Go7zFallback: false}, true)

		// Scenario C: Native Go with external fallback
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGo7z: true, Go7zFallback: true}, true)
	})

	// 5. Test extractArchive (SplitArchive disabled)
	t.Run("extractArchive SplitArchive disabled", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		a := unpack.Archive{Type: unpack.SplitArchive, MainFile: "test.001"}
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{}, false)
	})

	// 6. Test extractPendingArchives (external tool success to cover CommandLine and Output)
	t.Run("extractPendingArchives external success", func(t *testing.T) {
		if _, err := exec.LookPath("unrar"); err != nil {
			t.Skip("unrar binary not found in PATH")
		}
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		copyToDir(t, unpackFixture("single_rar5.rar"), job.DownloadDir)
		
		processed := make(map[string]bool)
		var allSuccessful []unpack.Archive
		var firstErr error
		
		pending, err := unpack.Scan(job.DownloadDir)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		
		opts := unpack.Options{
			UseGoRAR: false,
		}
		
		ok := u.extractPendingArchives(t.Context(), slog.Default(), job, pending, processed, opts, true, &firstErr, &allSuccessful)
		if !ok {
			t.Error("expected extractPendingArchives to return true")
		}
		if firstErr != nil {
			t.Errorf("expected nil error, got %v", firstErr)
		}
		if len(allSuccessful) != 1 {
			t.Errorf("len(allSuccessful) = %d; want 1", len(allSuccessful))
		}
	})

	// 7. Test extractPendingArchives containment violation
	t.Run("extractPendingArchives containment violation", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		copyToDir(t, unpackFixture("single_rar5.rar"), job.DownloadDir)
		
		// Create a symlink pointing outside DownloadDir to trigger containment error
		target := filepath.Join(job.DownloadDir, "bad_symlink")
		if err := os.Symlink(t.TempDir(), target); err != nil {
			t.Skipf("symlinks not supported on this OS: %v", err)
		}
		
		processed := make(map[string]bool)
		var allSuccessful []unpack.Archive
		var firstErr error
		
		pending, err := unpack.Scan(job.DownloadDir)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		
		opts := unpack.Options{
			UseGoRAR: true, // Go-native is fast and sufficient
		}
		
		ok := u.extractPendingArchives(t.Context(), slog.Default(), job, pending, processed, opts, true, &firstErr, &allSuccessful)
		// Since containment check fails, firstErr should be non-nil
		if firstErr == nil {
			t.Error("expected containment violation error, got nil")
		}
		if !job.UnpackError {
			t.Error("expected job.UnpackError to be true")
		}
		// Since containment check fails, the archive is not successfully finished, so ok should be false
		if ok {
			t.Error("expected ok=false since containment check failed")
		}
	})
}
