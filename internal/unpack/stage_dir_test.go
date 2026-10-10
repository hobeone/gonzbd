package unpack

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/testutil"
)

func assertNoStageDir(t *testing.T, outDir string) {
	t.Helper()
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", outDir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), unpackStagePrefix) {
			t.Fatalf("leftover stage dir %s found in %s", e.Name(), outDir)
		}
	}
}

func TestStagedSubprocessExtraction(t *testing.T) {
	t.Parallel()

	t.Run("prepareStageDir and publishStagedExtraction direct", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		staleDir := filepath.Join(outDir, unpackStagePrefix+"stale")
		if err := os.MkdirAll(staleDir, 0o750); err != nil {
			t.Fatalf("mkdir staleDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(staleDir, "partial.mkv"), []byte("trunc"), 0o600); err != nil {
			t.Fatalf("write partial.mkv: %v", err)
		}

		stageDir, err := prepareStageDir(outDir)
		if err != nil {
			t.Fatalf("prepareStageDir: %v", err)
		}
		defer os.RemoveAll(stageDir)

		if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
			t.Errorf("staleDir still exists after prepareStageDir: err=%v", err)
		}

		if err := os.MkdirAll(filepath.Join(stageDir, "sub"), 0o750); err != nil {
			t.Fatalf("mkdir stageDir/sub: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stageDir, "movie.mkv"), []byte("full-payload"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stageDir, "sub", "en.srt"), []byte("subs"), 0o600); err != nil {
			t.Fatalf("write sub/en.srt: %v", err)
		}

		var logBuf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logBuf, nil))
		published, err := publishStagedExtraction(log, outDir, stageDir, Options{OverwriteFiles: false})
		if err != nil || len(published) != 2 {
			t.Fatalf("publishStagedExtraction = %v, %v; want 2 files", published, err)
		}

		// Second stage with OverwriteFiles=false preserves existing movie.mkv,
		// logs the skip, and emits an OnLine trace.
		if err := os.WriteFile(filepath.Join(stageDir, "movie.mkv"), []byte("overwritten"), 0o600); err != nil {
			t.Fatalf("write movie.mkv v2: %v", err)
		}
		logBuf.Reset()
		var lines []string
		published, err = publishStagedExtraction(log, outDir, stageDir, Options{
			OverwriteFiles: false,
			OnLine:         func(line string) { lines = append(lines, line) },
		})
		if err != nil || len(published) != 0 {
			t.Fatalf("publishStagedExtraction(overwrite=false) = %v, %v; want 0", published, err)
		}
		if !strings.Contains(logBuf.String(), "skipping existing file") {
			t.Errorf("expected 'skipping existing file' log entry, got %q", logBuf.String())
		}
		if !slices.Contains(lines, "Skipping existing: movie.mkv") {
			t.Errorf("OnLine lines = %v, want 'Skipping existing: movie.mkv'", lines)
		}
		got, err := os.ReadFile(filepath.Join(outDir, "movie.mkv"))
		if err != nil || string(got) != "full-payload" {
			t.Fatalf("movie.mkv after overwrite=false = %q, %v; want full-payload", got, err)
		}

		// OverwriteFiles=true replaces existing movie.mkv.
		published, err = publishStagedExtraction(log, outDir, stageDir, Options{OverwriteFiles: true})
		if err != nil || len(published) != 1 {
			t.Fatalf("publishStagedExtraction(overwrite=true) = %v, %v; want 1", published, err)
		}
		got, err = os.ReadFile(filepath.Join(outDir, "movie.mkv"))
		if err != nil || string(got) != "overwritten" {
			t.Fatalf("movie.mkv after overwrite=true = %q, %v; want overwritten", got, err)
		}

		CleanupStageDirs("")
	})

	t.Run("UnRAR interrupted extraction does not leave partial payload in outDir", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		binDir := t.TempDir()
		failScript := filepath.Join(binDir, "unrar-fail.sh")
		testutil.WriteExecutable(t, failScript, "#!/bin/sh\nfor last; do :; done\nprintf 'truncated' > \"${last}movie.mkv\"\nexit 1\n")
		okScript := filepath.Join(binDir, "unrar-ok.sh")
		testutil.WriteExecutable(t, okScript, "#!/bin/sh\nfor last; do :; done\nprintf 'complete-rar-payload' > \"${last}movie.mkv\"\nexit 0\n")

		archive := Archive{
			Type:     RarArchive,
			Name:     "movie",
			MainFile: filepath.Join(binDir, "movie.rar"),
			Parts:    []string{filepath.Join(binDir, "movie.rar")},
		}
		discard := slog.New(slog.DiscardHandler)

		// 1. Interrupted/failing extraction must not leave a truncated movie.mkv or stageDir in outDir.
		if _, err := UnRAR(t.Context(), discard, archive, outDir, "", Options{UnrarCommand: failScript, OverwriteFiles: false}); err == nil {
			t.Fatal("expected error from failing unrar script")
		}
		if _, err := os.Stat(filepath.Join(outDir, "movie.mkv")); !os.IsNotExist(err) {
			t.Fatalf("truncated movie.mkv leaked into outDir after failed UnRAR: err=%v", err)
		}
		assertNoStageDir(t, outDir)

		// 2. Subsequent rerun with OverwriteFiles=false extracts and publishes the full file.
		res, err := UnRAR(t.Context(), discard, archive, outDir, "", Options{UnrarCommand: okScript, OverwriteFiles: false})
		if err != nil || len(res.ExtractedFiles) != 1 {
			t.Fatalf("UnRAR rerun = %+v, %v; want 1 extracted file", res, err)
		}
		assertNoStageDir(t, outDir)
		got, err := os.ReadFile(filepath.Join(outDir, "movie.mkv"))
		if err != nil || string(got) != "complete-rar-payload" {
			t.Fatalf("movie.mkv = %q, %v; want complete-rar-payload", got, err)
		}
	})

	t.Run("SevenZip interrupted extraction does not leave partial payload in outDir", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		binDir := t.TempDir()
		failScript := filepath.Join(binDir, "7z-fail.sh")
		scriptBody := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in -o*) out=\"${a#-o}\" ;; esac; done\nprintf 'truncated' > \"$out/movie.mkv\"\nexit 2\n"
		testutil.WriteExecutable(t, failScript, scriptBody)
		okScript := filepath.Join(binDir, "7z-ok.sh")
		okBody := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in -o*) out=\"${a#-o}\" ;; esac; done\nprintf 'complete-7z-payload' > \"$out/movie.mkv\"\nexit 0\n"
		testutil.WriteExecutable(t, okScript, okBody)

		archive := Archive{
			Type:     SevenZipArchive,
			Name:     "movie",
			MainFile: filepath.Join(binDir, "movie.7z"),
			Parts:    []string{filepath.Join(binDir, "movie.7z")},
		}
		discard := slog.New(slog.DiscardHandler)

		if _, err := SevenZip(t.Context(), discard, archive, outDir, "", Options{SevenZipCommand: failScript, OverwriteFiles: false}); err == nil {
			t.Fatal("expected error from failing 7z script")
		}
		if _, err := os.Stat(filepath.Join(outDir, "movie.mkv")); !os.IsNotExist(err) {
			t.Fatalf("truncated movie.mkv leaked into outDir after failed SevenZip: err=%v", err)
		}
		assertNoStageDir(t, outDir)

		res, err := SevenZip(t.Context(), discard, archive, outDir, "", Options{SevenZipCommand: okScript, OverwriteFiles: false})
		if err != nil || len(res.ExtractedFiles) != 1 {
			t.Fatalf("SevenZip rerun = %+v, %v; want 1 extracted file", res, err)
		}
		assertNoStageDir(t, outDir)
		got, err := os.ReadFile(filepath.Join(outDir, "movie.mkv"))
		if err != nil || string(got) != "complete-7z-payload" {
			t.Fatalf("movie.mkv = %q, %v; want complete-7z-payload", got, err)
		}
	})
}

func TestExternalExtract_PasswordReachesSubprocess(t *testing.T) {
	t.Parallel()

	t.Run("unrar", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		binDir := t.TempDir()
		script := filepath.Join(binDir, "unrar-pw.sh")
		body := "#!/bin/sh\nsaw_pw=0\nsaw_skip=0\nfor a in \"$@\"; do\n  case \"$a\" in\n    -psecret) saw_pw=1 ;;\n    -p\"<redacted>\") exit 3 ;;\n    -o-) saw_skip=1 ;;\n  esac\n  last=\"$a\"\ndone\nif [ \"$saw_pw\" -ne 1 ] || [ \"$saw_skip\" -ne 1 ]; then exit 2; fi\nprintf 'decrypted-rar' > \"${last}secret.txt\"\nexit 0\n"
		testutil.WriteExecutable(t, script, body)
		archive := Archive{
			Type:     RarArchive,
			Name:     "secret",
			MainFile: filepath.Join(binDir, "secret.rar"),
			Parts:    []string{filepath.Join(binDir, "secret.rar")},
		}
		res, err := UnRAR(t.Context(), slog.New(slog.DiscardHandler), archive, outDir, "secret", Options{
			UnrarCommand:   script,
			OverwriteFiles: false,
		})
		if err != nil {
			t.Fatalf("UnRAR with password failed: %v", err)
		}
		if !strings.Contains(res.CommandLine, "-p<redacted>") || strings.Contains(res.CommandLine, "-psecret") {
			t.Errorf("CommandLine = %q, want -p<redacted> and no -psecret", res.CommandLine)
		}
		got, err := os.ReadFile(filepath.Join(outDir, "secret.txt"))
		if err != nil || string(got) != "decrypted-rar" {
			t.Fatalf("secret.txt = %q, %v; want decrypted-rar", got, err)
		}
	})

	t.Run("7z", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		binDir := t.TempDir()
		script := filepath.Join(binDir, "7z-pw.sh")
		body := "#!/bin/sh\nsaw_pw=0\nsaw_skip=0\nfor a in \"$@\"; do\n  case \"$a\" in\n    -psecret) saw_pw=1 ;;\n    -p\"<redacted>\") exit 3 ;;\n    -aos) saw_skip=1 ;;\n    -o*) out=\"${a#-o}\" ;;\n  esac\ndone\nif [ \"$saw_pw\" -ne 1 ] || [ \"$saw_skip\" -ne 1 ]; then exit 2; fi\nprintf 'decrypted-7z' > \"$out/secret.txt\"\nexit 0\n"
		testutil.WriteExecutable(t, script, body)
		archive := Archive{
			Type:     SevenZipArchive,
			Name:     "secret",
			MainFile: filepath.Join(binDir, "secret.7z"),
			Parts:    []string{filepath.Join(binDir, "secret.7z")},
		}
		res, err := SevenZip(t.Context(), slog.New(slog.DiscardHandler), archive, outDir, "secret", Options{
			SevenZipCommand: script,
			OverwriteFiles:  false,
		})
		if err != nil {
			t.Fatalf("SevenZip with password failed: %v", err)
		}
		if !strings.Contains(res.CommandLine, "-p<redacted>") || strings.Contains(res.CommandLine, "-psecret") {
			t.Errorf("CommandLine = %q, want -p<redacted> and no -psecret", res.CommandLine)
		}
		got, err := os.ReadFile(filepath.Join(outDir, "secret.txt"))
		if err != nil || string(got) != "decrypted-7z" {
			t.Fatalf("secret.txt = %q, %v; want decrypted-7z", got, err)
		}
	})
}
