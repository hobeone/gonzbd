package postproc

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/cmdutil"
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

// TestUnpackStage_ExtractSingleRarGoMode_LabelsGoUnrarEngine verifies that
// OutputLines for a go_unrar extraction are labeled "[go_unrar]", not the
// generic "[unrar]" archive-type label, so the UI/log can distinguish which
// engine actually performed the extraction.
func TestUnpackStage_ExtractSingleRarGoMode_LabelsGoUnrarEngine(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	if err := enabledUnpackStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawGoUnrarLabel, sawBareUnrarLabel bool
	for _, line := range job.OutputLines {
		if strings.HasPrefix(line, "[go_unrar]") {
			sawGoUnrarLabel = true
		}
		if strings.HasPrefix(line, "[unrar]") {
			sawBareUnrarLabel = true
		}
	}
	if !sawGoUnrarLabel {
		t.Errorf("OutputLines missing [go_unrar] label: %v", job.OutputLines)
	}
	if sawBareUnrarLabel {
		t.Errorf("OutputLines contains generic [unrar] label for a go_unrar extraction: %v", job.OutputLines)
	}
}

// TestUnpackStage_CleanupDeletesArchiveParts verifies that when cleanup is
// enabled, the source RAR file is deleted after successful extraction.
func TestUnpackStage_CleanupDeletesArchiveParts(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	rarPath := copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	var output []string
	var mu sync.Mutex
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		defer mu.Unlock()
		output = append(output, fmt.Sprintf("[%s] %s", tool, line))
	}

	s := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true /* cleanup */)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(rarPath); !os.IsNotExist(err) {
		t.Errorf("archive still exists after cleanup: want removed, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, line := range output {
		if strings.Contains(line, "[unpack] Deleted archive file: single_rar5.rar") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected deleted archive file log, got: %v", output)
	}

	foundSummary := slices.Contains(job.OutputLines, "Cleaned up 1 archive file(s)")
	if !foundSummary {
		t.Errorf("expected 'Cleaned up 1 archive file(s)' in job.OutputLines, got: %v", job.OutputLines)
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

// buildTestTar writes a minimal .tar archive containing a single file entry
// into dir and returns its path.
func buildTestTar(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path) //nolint:gosec // test fixture path under t.TempDir()
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	tw := tar.NewWriter(f)
	content := []byte("tar payload content")
	hdr := &tar.Header{
		Name:    "payload.txt",
		Mode:    0o644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tar file: %v", err)
	}
	return path
}

// TestUnpackStage_ExtractTarArchive verifies a plain .tar archive is
// extracted successfully when EnableTar is true, and that extractTarArchive
// reports completion via job.OnOutput (proving the "err == nil && res.Err ==
// nil" success branch actually gates the "unpacking complete" callback,
// rather than firing unconditionally or never).
func TestUnpackStage_ExtractTarArchive(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	buildTestTar(t, dir, "archive.tar")

	var outputs []string
	job.OnOutput = func(_, line string) { outputs = append(outputs, line) }

	s := NewUnpackStageWith(unpack.Options{}, false)
	s.SetEnabled(true)
	s.EnableTar = true

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.UnpackError {
		t.Errorf("UnpackError = true; want false")
	}
	if _, err := os.Stat(filepath.Join(dir, "payload.txt")); err != nil {
		t.Errorf("expected payload.txt extracted from tar archive: %v", err)
	}

	var sawStart, sawLine, sawComplete bool
	for _, l := range outputs {
		if strings.Contains(l, "Unpacking:") && strings.Contains(l, "using go_tar") {
			sawStart = true
		}
		if strings.Contains(l, "Extracting") && strings.Contains(l, "payload.txt") {
			sawLine = true
		}
		if strings.Contains(l, "go_tar: unpacking complete") {
			sawComplete = true
		}
	}
	if !sawStart {
		t.Errorf("expected an OnOutput 'Unpacking: ... (using go_tar)' start message, got %v", outputs)
	}
	if !sawLine {
		t.Errorf("expected an OnOutput passthrough of GoTar's per-entry 'Extracting' line, got %v", outputs)
	}
	if !sawComplete {
		t.Errorf("expected an OnOutput 'go_tar: unpacking complete' message, got %v", outputs)
	}
}

// TestUnpackStage_TarDisabledSkips verifies that a tar archive is skipped
// entirely (not attempted) when EnableTar is false, mirroring the
// EnableFileJoin-disabled behavior for SplitArchive.
func TestUnpackStage_TarDisabledSkips(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	buildTestTar(t, dir, "archive.tar")

	s := NewUnpackStageWith(unpack.Options{}, false)
	s.SetEnabled(true)
	s.EnableTar = false

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.UnpackError {
		t.Errorf("UnpackError = true; want false (disabled tar should not be attempted)")
	}
	if _, err := os.Stat(filepath.Join(dir, "payload.txt")); err == nil {
		t.Errorf("payload.txt extracted even though EnableTar=false")
	}
	// The archive itself must still be present (never processed).
	if _, err := os.Stat(filepath.Join(dir, "archive.tar")); err != nil {
		t.Errorf("archive.tar removed/moved even though EnableTar=false: %v", err)
	}
}

// TestUnpackStage_MixedTarAndRarArchives verifies that a job download dir
// containing both a plain .tar and a RAR archive extracts both in a single
// UnpackStage.Run pass, each dispatched to its correct engine (go_tar for
// the tar, go_unrar for the RAR — proven via the per-engine OnOutput lines
// each dispatcher emits, since job.OutputLines labels tar with the generic
// "tar" archiveTypeName rather than "go_tar").
func TestUnpackStage_MixedTarAndRarArchives(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)
	buildTestTar(t, dir, "archive.tar")

	var outputs []string
	job.OnOutput = func(_, line string) { outputs = append(outputs, line) }

	s := enabledUnpackStage()
	s.EnableTar = true

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.UnpackError {
		t.Errorf("UnpackError = true; want false")
	}

	// RAR contents extracted.
	for _, name := range []string{"file1.txt", "file2.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("extracted RAR file %s not found: %v", name, err)
		}
	}
	// tar contents extracted.
	if _, err := os.Stat(filepath.Join(dir, "payload.txt")); err != nil {
		t.Errorf("extracted tar file payload.txt not found: %v", err)
	}

	var sawGoUnrar, sawGoTar bool
	for _, l := range outputs {
		if strings.Contains(l, "go_unrar") {
			sawGoUnrar = true
		}
		if strings.Contains(l, "go_tar") {
			sawGoTar = true
		}
	}
	if !sawGoUnrar {
		t.Errorf("expected an OnOutput line mentioning go_unrar (RAR dispatch), got %v", outputs)
	}
	if !sawGoTar {
		t.Errorf("expected an OnOutput line mentioning go_tar (tar dispatch), got %v", outputs)
	}
}

// buildTraversalTar writes a tar archive containing one legitimate entry
// ("safe.txt") alongside a path-traversal entry whose name attempts to
// escape outDir via "../../../etc/passwd-pwned". GoTar's type/path-sanitize
// logic (internal/unpack/go_tar.go) is expected to skip the traversal entry
// entirely — it should never be written to disk at all, inside or outside
// outDir — so the stage-level containment check
// (fsutil.CheckContainment/cleanupContainmentViolation in stage_unpack.go)
// never observes a violation to clean up.
func buildTraversalTar(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path) //nolint:gosec // test fixture path under t.TempDir()
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	tw := tar.NewWriter(f)
	entries := []struct {
		name    string
		content []byte
	}{
		{"safe.txt", []byte("safe content")},
		{"../../../etc/passwd-pwned", []byte("evil")},
	}
	for _, e := range entries {
		hdr := &tar.Header{
			Name:    e.name,
			Mode:    0o644,
			Size:    int64(len(e.content)),
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", e.name, err)
		}
		if _, err := tw.Write(e.content); err != nil {
			t.Fatalf("write tar content %q: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tar file: %v", err)
	}
	return path
}

// TestUnpackStage_TarTraversalDoesNotTripContainmentCleanup verifies that a
// tar containing a path-traversal entry, processed through the full
// UnpackStage.Run path, does NOT spuriously trip fsutil.CheckContainment
// (stage_unpack.go's post-extraction containment check) nor lose or
// corrupt the legitimate sibling output extracted from the same archive.
//
// This test does NOT and cannot exercise cleanupContainmentViolation's body
// (stage_unpack.go ~781): that function only runs when CheckContainment
// returns an error, and CheckContainment detects violations exclusively via
// filepath.EvalSymlinks — i.e. symlink escapes, not path-traversal-by-name.
// GoTar's SanitizeArchivePath (internal/unpack/go_tar.go) already rejects
// the traversal entry before any write occurs, so nothing is ever written
// outside outDir and CheckContainment never has anything to catch. GoTar
// also unconditionally skips all symlink/hardlink/device/FIFO entries (a
// deliberate design choice — see the tar hardening plan), so there is no
// tar-only way to construct a scenario that reaches
// cleanupContainmentViolation's body; that would require a genuine
// symlink-escape path through a different extractor (e.g. RAR/7z), which is
// out of scope here.
//
// What this test DOES prove: the extractor-level skip and the stage-level
// containment check are consistent — neither double-flags nor conflicts —
// and no violation is spuriously raised for a threat GoTar already fully
// neutralized before any write took place.
func TestUnpackStage_TarTraversalDoesNotTripContainmentCleanup(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	buildTraversalTar(t, dir, "traversal.tar")

	s := NewUnpackStageWith(unpack.Options{}, false)
	s.SetEnabled(true)
	s.EnableTar = true

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The containment check must never trip: the traversal entry was
	// skipped by GoTar itself, so nothing ever escaped outDir for the
	// stage-level check to catch.
	if job.UnpackError {
		t.Errorf("UnpackError = true; want false (no real containment violation occurred)")
	}

	// The legitimate sibling entry must survive intact — proving no
	// spurious containment-driven cleanup ran against the extraction.
	if _, err := os.Stat(filepath.Join(dir, "safe.txt")); err != nil {
		t.Errorf("safe.txt not extracted (or was wrongly cleaned up): %v", err)
	}

	// The traversal entry must not have escaped anywhere outside dir. The
	// archive entry name is "../../../etc/passwd-pwned" (three levels up
	// from outDir per filepath.Join semantics), so that is the path a
	// regression in SanitizeArchivePath would actually produce on disk.
	escaped := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(dir))), "etc", "passwd-pwned")
	if _, err := os.Stat(escaped); err == nil {
		t.Errorf("path traversal entry escaped to %s", escaped)
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
		{unpack.RarArchive, "rar"},
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

// assertExtractEngine checks that an extractArchive scenario against a
// nonexistent archive failed and was handled by the expected engine.
func assertExtractEngine(t *testing.T, scenario string, res unpack.Result, err error, wantEngine string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected error", scenario)
	}
	if res.Engine != wantEngine {
		t.Errorf("%s: Engine = %q, want %q", scenario, res.Engine, wantEngine)
	}
}

// writeStubBinary writes a minimal executable shell script to a temp file
// and returns its path. It exits non-zero so callers that dispatch to it
// (via an *Command override) exercise the real "external tool ran and
// failed" code path without depending on a real external binary being
// installed -- this repo's plain `go test ./...` CI job doesn't install
// unrar/7z/par2 (only `-tags=integration` does).
func writeStubBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "stubtool.*.tmp")
	if err != nil {
		t.Fatalf("create temp stub binary: %v", err)
	}
	if _, err := f.Write([]byte("#!/bin/sh\nexit 1\n")); err != nil {
		_ = f.Close()
		t.Fatalf("write stub binary: %v", err)
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		t.Fatalf("chmod stub binary: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close stub binary: %v", err)
	}
	stub := filepath.Join(dir, "stubtool")
	if err := os.Rename(f.Name(), stub); err != nil {
		t.Fatalf("rename stub binary: %v", err)
	}
	return stub
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
		// The fixture file is created at 0644, and 755 chmod'd onto a
		// regular file resolves to 0644 too (exec bits stripped), so the
		// mode is unchanged before/after -- OutputLines is the observable
		// that actually distinguishes "chmod ran" from "did nothing".
		if len(job.OutputLines) != 1 || !strings.Contains(job.OutputLines[0], "Applied permissions (755) to 1 file") {
			t.Errorf("OutputLines = %v; want confirmation of 1 file chmod'd", job.OutputLines)
		}
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
		resA, errA := u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGoRAR: true, GoRarFallback: false}, true, true)
		assertExtractEngine(t, "scenario A", resA, errA, "go_unrar")

		// Scenarios B and C exercise the external unrar path. Use a stub
		// binary rather than a skip-guard, so these scenarios always run
		// instead of silently losing coverage on any runner without a real
		// unrar installed -- this repo's plain `go test ./...` CI job
		// doesn't install unrar (only `-tags=integration` does).
		stub := writeStubBinary(t)

		// Scenario B: External only
		resB, errB := u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGoRAR: false, GoRarFallback: false, UnrarCommand: stub}, true, true)
		assertExtractEngine(t, "scenario B", resB, errB, "unrar")

		// Scenario C: Native Go with external fallback -- should behave like
		// B (fallback ran and switched engines away from go_unrar), proving
		// the fallback path actually executed rather than silently no-oping.
		resC, errC := u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGoRAR: true, GoRarFallback: true, UnrarCommand: stub}, true, true)
		assertExtractEngine(t, "scenario C", resC, errC, "unrar")
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
		resA, errA := u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGo7z: true, Go7zFallback: false}, true, true)
		assertExtractEngine(t, "scenario A", resA, errA, "go_7z")

		// Scenarios B and C exercise the external 7z path. Use a stub binary
		// (see the RAR subtest above) instead of a skip-guard.
		stub := writeStubBinary(t)

		// Scenario B: External only
		resB, errB := u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGo7z: false, Go7zFallback: false, SevenZipCommand: stub}, true, true)
		assertExtractEngine(t, "scenario B", resB, errB, "7zip")

		// Scenario C: Native Go with external fallback -- should behave like
		// B (fallback ran and switched engines away from go_7z).
		resC, errC := u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGo7z: true, Go7zFallback: true, SevenZipCommand: stub}, true, true)
		assertExtractEngine(t, "scenario C", resC, errC, "7zip")
	})

	// 5. Test extractArchive (SplitArchive disabled)
	t.Run("extractArchive SplitArchive disabled", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		a := unpack.Archive{Type: unpack.SplitArchive, MainFile: "test.001"}
		res, err := u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{}, false, false)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.Engine != "" || res.ExitCode != 0 || res.Err != nil {
			t.Errorf("expected zero-value Result for disabled join, got %+v", res)
		}
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

		ok := u.extractPendingArchives(t.Context(), slog.Default(), job, pending, processed, opts, true, true, &firstErr, &allSuccessful)
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

		ok := u.extractPendingArchives(t.Context(), slog.Default(), job, pending, processed, opts, true, true, &firstErr, &allSuccessful)
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

func TestUnpackStage_RealtimeLogTransitions(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	var mu sync.Mutex
	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		loggedLines = append(loggedLines, line)
		mu.Unlock()
	}

	if err := enabledUnpackStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	lines := slices.Clone(loggedLines)
	mu.Unlock()

	var sawStart, sawComplete bool
	for _, l := range lines {
		if strings.Contains(l, "Using go_unrar for RAR (pure-Go)") {
			sawStart = true
		}
		if strings.Contains(l, "go_unrar: unpacking complete") {
			sawComplete = true
		}
	}

	if !sawStart {
		t.Errorf("expected start log 'Using go_unrar for RAR (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawComplete {
		t.Errorf("expected complete log 'go_unrar: unpacking complete' in OnOutput, got: %v", loggedLines)
	}
}

func sevenZipFixture(name string) string {
	return filepath.Join("..", "unpack", "testdata", "sevenzip", name)
}

func TestUnpackStage_RealtimeLogTransitions_SevenZip(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, sevenZipFixture("lzma2.7z"), dir)

	var mu sync.Mutex
	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		loggedLines = append(loggedLines, line)
		mu.Unlock()
	}

	s := NewUnpackStageWith(unpack.Options{UseGo7z: true}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	lines := slices.Clone(loggedLines)
	mu.Unlock()

	var sawStart, sawComplete bool
	for _, l := range lines {
		if strings.Contains(l, "Using go_7z for 7-Zip (pure-Go)") {
			sawStart = true
		}
		if strings.Contains(l, "go_7z: unpacking complete") {
			sawComplete = true
		}
	}

	if !sawStart {
		t.Errorf("expected start log 'Using go_7z for 7-Zip (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawComplete {
		t.Errorf("expected complete log 'go_7z: unpacking complete' in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_RealtimeLogTransitions_SevenZip_Fallback(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	// Copy a RAR archive but name it as .7z to force GoSevenZip to fail
	// and trigger the fallback to external 7z.
	rarPath := copyToDir(t, unpackFixture("single_rar5.rar"), dir)
	fake7zPath := filepath.Join(dir, "single_rar5.7z")
	if err := os.Rename(rarPath, fake7zPath); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var mu sync.Mutex
	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
		mu.Unlock()
	}

	// We must configure Go7zFallback: true, UseGo7z: true, and ensure SevenZipCommand is set/detectable
	s := NewUnpackStageWith(unpack.Options{
		UseGo7z:         true,
		Go7zFallback:    true,
		SevenZipCommand: "7z",
	}, false)
	s.SetEnabled(true)

	// We expect this to fail eventually because it's not a real 7z archive.
	_ = s.Run(t.Context(), job)

	mu.Lock()
	lines := slices.Clone(loggedLines)
	mu.Unlock()

	var sawGo7zStart, sawGo7zFailRetry, sawExternal7zCommand bool
	for _, l := range lines {
		if strings.Contains(l, "[go_7z] Using go_7z for 7-Zip (pure-Go)") {
			sawGo7zStart = true
		}
		if strings.Contains(l, "[go_7z] Go-native extraction failed:") && strings.Contains(l, "retrying with 7z") {
			sawGo7zFailRetry = true
		}
		if strings.Contains(l, "[7z] Running command:") {
			sawExternal7zCommand = true
		}
	}

	if !sawGo7zStart {
		t.Errorf("expected start log 'Using go_7z for 7-Zip (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawGo7zFailRetry {
		t.Errorf("expected fallback retry log in OnOutput, got: %v", loggedLines)
	}
	if !sawExternal7zCommand {
		t.Errorf("expected external command log in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_RealtimeLogTransitions_Rar_Fallback(t *testing.T) {
	t.Parallel()

	// Check if unrar is available on this system first.
	if _, lookErr := exec.LookPath("unrar"); lookErr != nil {
		t.Skip("unrar binary not found, skipping fallback test")
	}

	job, dir := stageJob(t)
	// Use a corrupt rar to trigger GoUnRAR failure and fallback to unrar
	copyToDir(t, unpackFixture("corrupt.rar"), dir)

	var mu sync.Mutex
	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
		mu.Unlock()
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGoRAR:      true,
		GoRarFallback: true,
		UnrarCommand:  "unrar",
	}, false)
	s.SetEnabled(true)

	_ = s.Run(t.Context(), job)

	mu.Lock()
	lines := slices.Clone(loggedLines)
	mu.Unlock()

	var sawGoRarStart, sawGoRarFailRetry, sawExternalUnrarCommand bool
	for _, l := range lines {
		if strings.Contains(l, "[go_unrar] Using go_unrar for RAR (pure-Go)") {
			sawGoRarStart = true
		}
		if strings.Contains(l, "[go_unrar] Go-native extraction failed:") && strings.Contains(l, "retrying with unrar") {
			sawGoRarFailRetry = true
		}
		if strings.Contains(l, "[unrar] Running command:") {
			sawExternalUnrarCommand = true
		}
	}

	if !sawGoRarStart {
		t.Errorf("expected start log 'Using go_unrar for RAR (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawGoRarFailRetry {
		t.Errorf("expected fallback retry log in OnOutput, got: %v", loggedLines)
	}
	if !sawExternalUnrarCommand {
		t.Errorf("expected external command log in OnOutput, got: %v", loggedLines)
	}
}

func createDummyExecutable(t *testing.T, dir, filename, content string) string {
	t.Helper()
	f, err := os.CreateTemp(dir, filename+".*.tmp")
	if err != nil {
		t.Fatalf("create temp dummy executable: %v", err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		_ = f.Close()
		t.Fatalf("write dummy executable: %v", err)
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		t.Fatalf("chmod dummy executable: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close dummy executable: %v", err)
	}
	target := filepath.Join(dir, filename)
	if err := os.Rename(f.Name(), target); err != nil {
		t.Fatalf("rename dummy executable: %v", err)
	}
	return target
}

func TestUnpackStage_RealtimeLogTransitions_Rar_Fallback_Success(t *testing.T) {
	t.Parallel()

	tmpBinDir := t.TempDir()
	unrarPath := filepath.Join(tmpBinDir, "mock-unrar")

	successScript := `#!/bin/sh
echo "unrar stdout line 1"
echo "unrar stdout line 2"
outdir=""
for arg in "$@"; do
  case "$arg" in
    -o*) outdir="${arg#-o}" ;;
    *) outdir="$arg" ;;
  esac
done
outdir="${outdir%/}"
if [ -n "$outdir" ] && [ -d "$outdir" ]; then
  touch "$outdir/mock_extracted_file.txt"
fi
exit 0
`
	createDummyExecutable(t, tmpBinDir, "mock-unrar", successScript)

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("corrupt.rar"), dir)

	var mu sync.Mutex
	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
		mu.Unlock()
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGoRAR:      true,
		GoRarFallback: true,
		UnrarCommand:  unrarPath,
	}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}

	if job.UnpackError {
		t.Error("expected job.UnpackError to be false since fallback succeeded")
	}

	mu.Lock()
	lines := slices.Clone(loggedLines)
	mu.Unlock()

	var sawGoRarStart, sawGoRarFailRetry, sawExternalUnrarCommand, sawStdout, sawSuccess bool
	for _, l := range lines {
		if strings.Contains(l, "[go_unrar] Using go_unrar for RAR (pure-Go)") {
			sawGoRarStart = true
		}
		if strings.Contains(l, "[go_unrar] Go-native extraction failed:") && strings.Contains(l, "retrying with") {
			sawGoRarFailRetry = true
		}
		if strings.Contains(l, "[unrar] Running command:") {
			sawExternalUnrarCommand = true
		}
		if strings.Contains(l, "[unrar] unrar stdout line 1") {
			sawStdout = true
		}
		if strings.Contains(l, "[unrar] Unpacking complete:") {
			sawSuccess = true
		}
	}

	if !sawGoRarStart {
		t.Errorf("missing go_unrar start log: %v", loggedLines)
	}
	if !sawGoRarFailRetry {
		t.Errorf("missing fallback retry log: %v", loggedLines)
	}
	if !sawExternalUnrarCommand {
		t.Errorf("missing external command log: %v", loggedLines)
	}
	if !sawStdout {
		t.Errorf("missing stdout line: %v", loggedLines)
	}
	if !sawSuccess {
		t.Errorf("missing success log: %v", loggedLines)
	}
}

func TestUnpackStage_RealtimeLogTransitions_SevenZip_Fallback_Success(t *testing.T) {
	t.Parallel()

	tmpBinDir := t.TempDir()
	dummyScript := `#!/bin/sh
echo "7z stdout line 1"
echo "7z stdout line 2"
outdir=""
for arg in "$@"; do
  case "$arg" in
    -o*) outdir="${arg#-o}" ;;
    *) outdir="$arg" ;;
  esac
done
outdir="${outdir%/}"
if [ -n "$outdir" ] && [ -d "$outdir" ]; then
  touch "$outdir/mock_extracted_file.txt"
fi
exit 0
`
	szPath := createDummyExecutable(t, tmpBinDir, "mock-7z", dummyScript)

	job, dir := stageJob(t)
	// Copy a RAR archive but name it as .7z to force GoSevenZip to fail
	rarPath := copyToDir(t, unpackFixture("single_rar5.rar"), dir)
	fake7zPath := filepath.Join(dir, "single_rar5.7z")
	if err := os.Rename(rarPath, fake7zPath); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var mu sync.Mutex
	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
		mu.Unlock()
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGo7z:         true,
		Go7zFallback:    true,
		SevenZipCommand: szPath,
	}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}

	if job.UnpackError {
		t.Error("expected job.UnpackError to be false since fallback succeeded")
	}

	mu.Lock()
	lines := slices.Clone(loggedLines)
	mu.Unlock()

	var sawGo7zStart, sawGo7zFailRetry, sawExternal7zCommand, sawStdout, sawSuccess bool
	for _, l := range lines {
		if strings.Contains(l, "[go_7z] Using go_7z for 7-Zip (pure-Go)") {
			sawGo7zStart = true
		}
		if strings.Contains(l, "[go_7z] Go-native extraction failed:") && strings.Contains(l, "retrying with") {
			sawGo7zFailRetry = true
		}
		if strings.Contains(l, "[7z] Running command:") {
			sawExternal7zCommand = true
		}
		if strings.Contains(l, "[7z] 7z stdout line 1") {
			sawStdout = true
		}
		if strings.Contains(l, "[7z] Unpacking complete:") {
			sawSuccess = true
		}
	}

	if !sawGo7zStart {
		t.Errorf("expected start log 'Using go_7z for 7-Zip (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawGo7zFailRetry {
		t.Errorf("expected fallback retry log in OnOutput, got: %v", loggedLines)
	}
	if !sawExternal7zCommand {
		t.Errorf("expected external command log in OnOutput, got: %v", loggedLines)
	}
	if !sawStdout {
		t.Errorf("expected stdout line in OnOutput, got: %v", loggedLines)
	}
	if !sawSuccess {
		t.Errorf("expected success log in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_RealtimeLogTransitions_SevenZip_Direct_Success(t *testing.T) {
	t.Parallel()

	tmpBinDir := t.TempDir()
	dummyScript := `#!/bin/sh
echo "7z stdout line 1"
exit 0
`
	szPath := createDummyExecutable(t, tmpBinDir, "mock-7z", dummyScript)

	job, dir := stageJob(t)
	copyToDir(t, sevenZipFixture("lzma2.7z"), dir)

	var mu sync.Mutex
	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
		mu.Unlock()
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGo7z:         false,
		SevenZipCommand: szPath,
	}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("expected extraction to succeed, got error: %v", err)
	}

	mu.Lock()
	lines := slices.Clone(loggedLines)
	mu.Unlock()

	var sawStart, sawComplete bool
	for _, l := range lines {
		if strings.Contains(l, "[7z] Unpacking:") {
			sawStart = true
		}
		if strings.Contains(l, "[7z] Unpacking complete:") {
			sawComplete = true
		}
	}

	if !sawStart {
		t.Errorf("expected start log 'Unpacking:' in OnOutput, got: %v", loggedLines)
	}
	if !sawComplete {
		t.Errorf("expected complete log 'Unpacking complete:' in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_SevenZip_ExternalFallback_SetsCommand(t *testing.T) {
	tmpBinDir := t.TempDir()
	dummyScript := `#!/bin/sh
echo "7z stdout line 1"
exit 0
`
	szPath := createDummyExecutable(t, tmpBinDir, "mock-7z", dummyScript)
	t.Setenv("GONZBD_SEVENZIP_BIN", szPath)

	job, dir := stageJob(t)
	copyToDir(t, sevenZipFixture("lzma2.7z"), dir)

	s := NewUnpackStageWith(unpack.Options{
		UseGo7z:         false,
		SevenZipCommand: "", // empty initially, should be discovered from GONZBD_SEVENZIP_BIN and assigned in onExternal
	}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("expected extraction to succeed with env var binary, got error: %v", err)
	}
}

func TestUnpackStage_RealtimeLogTransitions_Rar_Direct_Success(t *testing.T) {
	t.Parallel()

	tmpBinDir := t.TempDir()
	dummyScript := `#!/bin/sh
echo "unrar stdout line 1"
exit 0
`
	unrarPath := createDummyExecutable(t, tmpBinDir, "mock-unrar", dummyScript)

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	var mu sync.Mutex
	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
		mu.Unlock()
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGoRAR:     false,
		UnrarCommand: unrarPath,
	}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("expected extraction to succeed, got error: %v", err)
	}

	mu.Lock()
	lines := slices.Clone(loggedLines)
	mu.Unlock()

	var sawStart, sawComplete bool
	for _, l := range lines {
		if strings.Contains(l, "[unrar] Unpacking:") {
			sawStart = true
		}
		if strings.Contains(l, "[unrar] Unpacking complete:") {
			sawComplete = true
		}
	}

	if !sawStart {
		t.Errorf("expected start log 'Unpacking:' in OnOutput, got: %v", loggedLines)
	}
	if !sawComplete {
		t.Errorf("expected complete log 'Unpacking complete:' in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_ContainmentViolation_ImmediateCleanup(t *testing.T) {
	t.Parallel()

	if _, lookErr := exec.LookPath("sh"); lookErr != nil {
		t.Skip("sh not found, skipping script-based containment cleanup test")
	}

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	outsideDir := t.TempDir()
	escapedFile := filepath.Join(outsideDir, "secret_outside.txt")
	if err := os.WriteFile(escapedFile, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write escaped file: %v", err)
	}

	scriptPath := filepath.Join(t.TempDir(), "fake_unrar.sh")
	scriptContent := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  outDir="$arg"
done
outDir=$(echo "$outDir" | sed 's#/$##')
ln -s "%s" "$outDir/evil_link"
touch "$outDir/normal_file.txt"
exit 0
`, escapedFile)
	createDummyExecutable(t, t.TempDir(), "fake_unrar.sh", scriptContent)

	s := NewUnpackStageWith(unpack.Options{
		UseGoRAR:     false,
		UnrarCommand: scriptPath,
	}, true)
	s.SetEnabled(true)

	_ = s.Run(t.Context(), job)

	if !job.UnpackError {
		t.Error("expected job.UnpackError = true when containment check fails")
	}

	if _, err := os.Lstat(filepath.Join(dir, "evil_link")); !os.IsNotExist(err) {
		t.Errorf("evil_link inside DownloadDir should be unlinked immediately, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "normal_file.txt")); !os.IsNotExist(err) {
		t.Errorf("normal_file.txt inside DownloadDir should be deleted immediately, stat err: %v", err)
	}
	// The symlink target lives outside DownloadDir and is a pre-existing file.
	// It MUST survive: following evil_link to delete it is the vulnerability —
	// a malicious archive could otherwise erase any file the process can reach.
	if _, err := os.Stat(escapedFile); err != nil {
		t.Errorf("escaped symlink target %s outside DownloadDir must be preserved, stat err: %v", escapedFile, err)
	}
}

func TestCleanupContainmentViolation_Direct(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	symlinkPath := filepath.Join(outDir, "link.txt")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	normalFile := filepath.Join(outDir, "normal.txt")
	if err := os.WriteFile(normalFile, []byte("normal content"), 0o644); err != nil {
		t.Fatalf("write normal file: %v", err)
	}

	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, nil)
	log := slog.New(handler)

	// We pass:
	// 1. "link.txt" (relative path to symlink pointing out of bounds)
	// 2. normalFile (absolute path to normal file inside outDir)
	// 3. "nonexistent.txt" (to cover error path when file doesn't exist)
	cleanupContainmentViolation(outDir, []string{"link.txt", normalFile, "nonexistent.txt"}, log)

	// The out-of-bounds symlink target is a pre-existing victim file: it MUST
	// be preserved. Following the symlink to delete it would let a malicious
	// archive destroy arbitrary files by planting a symlink pointing at them.
	if _, err := os.Stat(outsideFile); err != nil {
		t.Errorf("out-of-bounds symlink target %s must be preserved, stat err: %v", outsideFile, err)
	}
	// The escaping symlink itself (inside outDir) must be unlinked.
	if _, err := os.Lstat(symlinkPath); !os.IsNotExist(err) {
		t.Errorf("expected escaping symlink %s to be unlinked, stat err: %v", symlinkPath, err)
	}
	if _, err := os.Stat(normalFile); !os.IsNotExist(err) {
		t.Errorf("expected normal file %s to be deleted, stat err: %v", normalFile, err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "removed extracted file after containment check failure") {
		t.Errorf("expected log to contain 'removed extracted file after containment check failure', got: %s", logs)
	}

	// Also test with nil logger to ensure no panic on logging branches
	if err := os.WriteFile(normalFile, []byte("again"), 0o644); err != nil {
		t.Fatalf("write normal file again: %v", err)
	}
	cleanupContainmentViolation(outDir, []string{"normal.txt"}, nil)
	if _, err := os.Stat(normalFile); !os.IsNotExist(err) {
		t.Errorf("expected normal file to be deleted with nil logger, stat err: %v", err)
	}
}

func TestUnpackStage_ApplyStrictSandbox(t *testing.T) {
	s := NewUnpackStageWith(unpack.Options{
		Sandbox: cmdutil.SandboxConfig{
			Enabled: true,
			Strict:  true,
		},
	}, false)

	s.Apply(UnpackConfig{Base: unpack.Options{Sandbox: cmdutil.SandboxConfig{Enabled: true, Strict: false}}})
	s.mu.RLock()
	strict := s.BaseOpts.Sandbox.Strict
	s.mu.RUnlock()
	if strict {
		t.Error("expected Apply with Strict=false to update BaseOpts.Sandbox.Strict")
	}

	s.Apply(UnpackConfig{Base: unpack.Options{Sandbox: cmdutil.SandboxConfig{Enabled: true, Strict: true}}})
	job := &Job{DownloadDir: "/tmp/test-sandbox-target", Queue: &queue.Job{}}
	opts := s.prepareOptions(t.Context(), slog.Default(), job, s.BaseOpts, "")
	if opts.Sandbox.TargetDir != "/tmp/test-sandbox-target" {
		t.Errorf("expected prepareOptions to set TargetDir to %q, got %q", job.DownloadDir, opts.Sandbox.TargetDir)
	}
}

func TestExtractWithDriver_GoModeAndFallback(t *testing.T) {
	t.Parallel()

	t.Run("go mode success without fallback", func(t *testing.T) {
		t.Parallel()
		job, _ := stageJob(t)
		var runGoCalled, runExtCalled bool

		driver := archiveEngineDriver{
			toolName:   "mocktool",
			goToolName: "go_mocktool",
			formatName: "MOCK",
			useGo:      true,
			fallback:   false,
			findBin: func(_ unpack.Options) (string, error) {
				return "/bin/mocktool", nil
			},
			runGo: func(_ context.Context, _ *slog.Logger, _ unpack.Archive, _ string, _ unpack.Options) (unpack.Result, error) {
				runGoCalled = true
				return unpack.Result{Engine: "go_mocktool"}, nil
			},
			runExternal: func(_ context.Context, _ *slog.Logger, _ unpack.Archive, _ string, _ unpack.Options) (unpack.Result, error) {
				runExtCalled = true
				return unpack.Result{}, nil
			},
		}

		res, err := extractWithDriver(t.Context(), slog.Default(), job, unpack.Archive{Name: "test.mock"}, unpack.Options{}, driver)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !runGoCalled || runExtCalled {
			t.Fatalf("expected runGo=true, runExt=false, got runGo=%v, runExt=%v", runGoCalled, runExtCalled)
		}
		if res.Engine != "go_mocktool" {
			t.Fatalf("expected engine go_mocktool, got %s", res.Engine)
		}
	})

	t.Run("go mode failure triggers fallback to external when binary available", func(t *testing.T) {
		t.Parallel()
		job, _ := stageJob(t)
		var runGoCalled, runExtCalled bool
		var extBinReceived string

		driver := archiveEngineDriver{
			toolName:   "mocktool",
			goToolName: "go_mocktool",
			formatName: "MOCK",
			useGo:      true,
			fallback:   true,
			findBin: func(_ unpack.Options) (string, error) {
				return "/usr/local/bin/mocktool", nil
			},
			runGo: func(_ context.Context, _ *slog.Logger, _ unpack.Archive, _ string, _ unpack.Options) (unpack.Result, error) {
				runGoCalled = true
				return unpack.Result{}, errors.New("go engine error")
			},
			runExternal: func(_ context.Context, _ *slog.Logger, _ unpack.Archive, _ string, _ unpack.Options) (unpack.Result, error) {
				runExtCalled = true
				return unpack.Result{Engine: "mocktool"}, nil
			},
			onExternal: func(_ *unpack.Options, bin string) {
				extBinReceived = bin
			},
		}

		res, err := extractWithDriver(t.Context(), slog.Default(), job, unpack.Archive{Name: "test.mock"}, unpack.Options{}, driver)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !runGoCalled || !runExtCalled {
			t.Fatalf("expected both runGo and runExt called, got runGo=%v, runExt=%v", runGoCalled, runExtCalled)
		}
		if extBinReceived != "/usr/local/bin/mocktool" {
			t.Fatalf("expected extBin=/usr/local/bin/mocktool, got %q", extBinReceived)
		}
		if res.Engine != "mocktool" {
			t.Fatalf("expected engine mocktool, got %s", res.Engine)
		}
	})
}
