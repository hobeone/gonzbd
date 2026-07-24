package postproc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/par2"
)

// writeScript creates an executable script file at path. To avoid the
// transient ETXTBSY ("text file busy") error that occurs when fork/exec
// races with a recently-written file (particularly under the race
// detector), we write to a temporary file in the same directory and
// then rename it into place. The exec target was never open for
// writing in this process, so the kernel cannot return ETXTBSY.
func writeScript(t *testing.T, path string, content []byte) {
	t.Helper()
	dir := filepath.Dir(path)
	//nolint:gosec // G306: test-only script needs exec bit
	f, err := os.CreateTemp(dir, ".script-*.tmp")
	if err != nil {
		t.Fatalf("create temp script: %v", err)
	}
	tmpName := f.Name()
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(tmpName)
		t.Fatalf("write script: %v", err)
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		os.Remove(tmpName)
		t.Fatalf("chmod script: %v", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpName)
		t.Fatalf("close script: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		t.Fatalf("rename script: %v", err)
	}
}

// stageJob builds a *Job with a fresh tmp DownloadDir and a minimal
// queue.Job. Tests that need files in the DownloadDir can add them via
// the returned path.
func stageJob(t *testing.T) (*Job, string) {
	t.Helper()
	dir := t.TempDir()
	qjob := newQueueJob(t, "test-job-id", 0)
	qjob.Name = "test.job"
	qjob.Filename = "test.nzb"
	return &Job{
		Queue:       qjob,
		DownloadDir: dir,
	}, dir
}

func TestRepairStage_Name(t *testing.T) {
	t.Parallel()
	if got := (&RepairStage{}).Name(); got != "repair" {
		t.Errorf("Name = %q; want %q", got, "repair")
	}
}

func TestRepairStage_EmptyDir(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	if err := NewRepairStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run on empty dir: %v", err)
	}
	if job.ParError {
		t.Errorf("ParError = true; want false")
	}
}

// P22: RepairStage.Run skips par2 when QuickCheckPassed is set.
func TestRepairStage_SkippedWhenQuickCheckPassed(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	job.QuickCheckPassed = true

	// Create a par2 file so the stage has something to find, but should skip.
	os.WriteFile(filepath.Join(job.DownloadDir, "movie.par2"), []byte("fake"), 0o644)

	stage := NewRepairStage()
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The stage should skip — no ParError should be set.
	if job.ParError {
		t.Error("ParError should be false when QuickCheckPassed")
	}
	// Verify the skip was logged in OutputLines.
	found := false
	for _, line := range job.OutputLines {
		if strings.Contains(line, "QuickCheck") || strings.Contains(line, "skip") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected OutputLines to contain QuickCheck skip message, got %v", job.OutputLines)
	}
}

// TestRepairStage_SkippedWhenDirectUnpackSucceeded verifies that RepairStage.Run skips par2
// when Direct Unpack successfully extracted all archives.
func TestRepairStage_SkippedWhenDirectUnpackSucceeded(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)

	// Populate DirectUnpackSets and keep Failures empty.
	job.DirectUnpackSets = map[string]directunpack.SuccessSet{
		"movie": {
			RarParts:       []string{"movie.part01.rar"},
			ExtractedFiles: []string{"movie.mkv"},
		},
	}
	job.DirectUnpackFailures = nil

	// Create a par2 file so the stage has something to find, but it should skip before even scanning.
	os.WriteFile(filepath.Join(job.DownloadDir, "movie.par2"), []byte("fake"), 0o644)

	stage := NewRepairStage()
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The stage should skip — no ParError should be set.
	if job.ParError {
		t.Error("ParError should be false when DirectUnpack succeeded")
	}

	// Verify the skip was logged in OutputLines.
	found := false
	for _, line := range job.OutputLines {
		if strings.Contains(line, "Direct Unpack successfully extracted") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected OutputLines to contain Direct Unpack skip message, got %v", job.OutputLines)
	}
}

// Regression test: par2 files must be deleted by the par2_cleanup stage
// after unpack succeeds. Previously cleanup happened inside RepairStage
// before unpack even ran.
func TestPar2CleanupStage_RunsAfterSuccessfulUnpack(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)

	// Create data + par2 files.
	os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("video"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.par2"), []byte("par2 main"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.vol00+1.par2"), []byte("par2 vol"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.vol01+2.par2"), []byte("par2 vol"), 0o644)
	// Create backup copy file.
	os.WriteFile(filepath.Join(dir, "movie.mkv.1"), []byte("backup"), 0o644)

	// Simulate: repair succeeded, unpack succeeded (defaults).
	stage := NewPar2CleanupStage(true)
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The data file must survive.
	if _, err := os.Stat(filepath.Join(dir, "movie.mkv")); err != nil {
		t.Error("movie.mkv should survive cleanup")
	}

	// All par2 files should be deleted.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(strings.ToLower(e.Name()), ".par2") {
			t.Errorf("par2 file %q should have been cleaned up", e.Name())
		}
	}

	// Verify OutputLines logs.
	foundPar2Cleaned := false
	foundBackupsCleaned := false
	expectedLogs := map[string]bool{
		"[par2_cleanup] Deleted par2 file: movie.par2":         false,
		"[par2_cleanup] Deleted par2 file: movie.vol00+1.par2": false,
		"[par2_cleanup] Deleted par2 file: movie.vol01+2.par2": false,
		"[par2_cleanup] Deleted par2 backup file: movie.mkv.1": false,
	}
	for _, line := range job.OutputLines {
		if line == "Cleaned up 3 par2 file(s)" {
			foundPar2Cleaned = true
		}
		if line == "Cleaned up 1 par2 backup file(s)" {
			foundBackupsCleaned = true
		}
		if _, expected := expectedLogs[line]; expected {
			expectedLogs[line] = true
		}
	}
	if !foundPar2Cleaned {
		t.Errorf("expected 'Cleaned up 3 par2 file(s)' in output, got: %v", job.OutputLines)
	}
	if !foundBackupsCleaned {
		t.Errorf("expected 'Cleaned up 1 par2 backup file(s)' in output, got: %v", job.OutputLines)
	}
	for expectedLine, found := range expectedLogs {
		if !found {
			t.Errorf("expected log line %q not found in: %v", expectedLine, job.OutputLines)
		}
	}
}

// Par2 cleanup must be skipped when unpack failed.
func TestPar2CleanupStage_SkippedOnUnpackError(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)
	job.UnpackError = true

	par2File := filepath.Join(dir, "movie.par2")
	os.WriteFile(par2File, []byte("par2"), 0o644)

	stage := NewPar2CleanupStage(true)
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Par2 file should survive because unpack failed.
	if _, err := os.Stat(par2File); err != nil {
		t.Error("par2 file should be preserved when unpack fails")
	}
}

// Par2 cleanup must be skipped when repair failed.
func TestPar2CleanupStage_SkippedOnParError(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)
	job.ParError = true

	par2File := filepath.Join(dir, "movie.par2")
	os.WriteFile(par2File, []byte("par2"), 0o644)

	stage := NewPar2CleanupStage(true)
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Par2 file should survive because repair failed.
	if _, err := os.Stat(par2File); err != nil {
		t.Error("par2 file should be preserved when repair fails")
	}
}

// Par2 cleanup is a no-op when Cleanup=false.
func TestPar2CleanupStage_DisabledByConfig(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)

	par2File := filepath.Join(dir, "movie.par2")
	os.WriteFile(par2File, []byte("par2"), 0o644)

	stage := NewPar2CleanupStage(false) // cleanup disabled
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(par2File); err != nil {
		t.Error("par2 file should be preserved when cleanup is disabled")
	}
}

// P22: ScriptStage with ScriptCanFail=true swallows non-zero exit errors.
func TestScriptStage_ScriptCanFail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)
	job.Queue.Script = "fail.sh"

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "fail.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 3\n"))

	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")
	stage.SetScriptCanFail(true)

	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Errorf("Run with ScriptCanFail=true should return nil, got %v", err)
	}
}

// TestRepairStage_NoCleanupInRepair verifies that the repair stage itself
// no longer deletes par2 files — that's now Par2CleanupStage's job.
func TestRepairStage_NoCleanupInRepair(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("video data"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.par2"), []byte("par2 main"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.vol000+01.par2"), []byte("par2 vol"), 0o644)

	qjob := newQueueJob(t, "test-par-cleanup", 0)
	qjob.Name = "movie"
	job := &Job{
		Queue:       qjob,
		DownloadDir: dir,
	}

	stage := NewRepairStageWith(par2.RunOptions{Command: "/nonexistent/par2"})
	_ = stage.Run(t.Context(), job)

	// Par2 files should still exist — repair stage no longer deletes them.
	if _, err := os.Stat(filepath.Join(dir, "movie.par2")); err != nil {
		t.Error("movie.par2 should survive repair stage (cleanup moved to par2_cleanup)")
	}
	if _, err := os.Stat(filepath.Join(dir, "movie.mkv")); err != nil {
		t.Error("movie.mkv should still exist")
	}
}

func TestUnpackStage_Name(t *testing.T) {
	t.Parallel()
	if got := (&UnpackStage{}).Name(); got != "unpack" {
		t.Errorf("Name = %q; want %q", got, "unpack")
	}
}

func TestUnpackStage_EmptyDir(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	if err := NewUnpackStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run on empty dir: %v", err)
	}
	if job.UnpackError {
		t.Errorf("UnpackError = true; want false")
	}
}

func TestUnpackStage_NoKnownArchives(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)
	// Files with unrecognized extensions are classified as unknown and
	// skipped by Scan; no archive means no error.
	for _, name := range []string{"readme.txt", "info.nfo", "data.bin"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := NewUnpackStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.UnpackError {
		t.Errorf("UnpackError set for unknown files")
	}
}

func TestRecoverPar2NamesStage_Name(t *testing.T) {
	t.Parallel()
	if got := (&RecoverPar2NamesStage{}).Name(); got != "recover_par2_names" {
		t.Errorf("Name = %q; want %q", got, "recover_par2_names")
	}
}

func TestRecoverPar2NamesStage_EmptyDir(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	if err := NewRecoverPar2NamesStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run on empty dir: %v", err)
	}
}

func TestDeobfuscateStage_Name(t *testing.T) {
	t.Parallel()
	if got := (&DeobfuscateStage{}).Name(); got != "deobfuscate" {
		t.Errorf("Name = %q; want %q", got, "deobfuscate")
	}
}

func TestDeobfuscateStage_EmptyDir(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	if err := NewDeobfuscateStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run on empty dir: %v", err)
	}
}

func TestScriptStage_Name(t *testing.T) {
	t.Parallel()
	if got := (&ScriptStage{}).Name(); got != "script" {
		t.Errorf("Name = %q; want %q", got, "script")
	}
}

func TestScriptStage_EmptyScriptSkipped(t *testing.T) {
	t.Parallel()
	cases := []string{"", "None"}
	for _, script := range cases {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			job, _ := stageJob(t)
			job.Queue.Script = script
			stage := NewScriptStage("/nonexistent", "/tmp/complete", "test", "", "")
			if err := stage.Run(t.Context(), job); err != nil {
				t.Errorf("Run with script=%q returned %v; want nil", script, err)
			}
		})
	}
}

func TestScriptStage_SuccessfulScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)
	job.Queue.Script = "ok.sh"

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "ok.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 0\n"))

	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "key", "http://x")
	if err := stage.Run(t.Context(), job); err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestScriptStage_FailingScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)
	job.Queue.Script = "fail.sh"

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "fail.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 7\n"))

	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")
	err := stage.Run(t.Context(), job)
	if err == nil {
		t.Fatalf("expected error for exit 7; got nil")
	}
	if !strings.Contains(err.Error(), "exited 7") {
		t.Errorf("err = %v; want contains 'exited 7'", err)
	}
}

func TestScriptStage_StatusFlagsFromJob(t *testing.T) {
	// Verifies job.ParError / UnpackError / FailMsg translate to pp_status=1.
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "capture.sh")
	outPath := filepath.Join(scriptDir, "status.out")
	script := "#!/bin/sh\necho \"$SAB_PP_STATUS\" > " + outPath + "\n"
	writeScript(t, scriptPath, []byte(script))

	job, _ := stageJob(t)
	job.Queue.Script = "capture.sh"
	job.ParError = true // should push status to 1

	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if want := "1\n"; string(got) != want {
		t.Errorf("SAB_PP_STATUS = %q; want %q", string(got), want)
	}
}

func TestScriptStage_AbsolutePathOverridesScriptDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)

	// Script field is an absolute path; must be rejected for security.
	scriptPath := filepath.Join(t.TempDir(), "abs.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 0\n"))
	job.Queue.Script = scriptPath

	stage := NewScriptStage("/nonexistent-dir", "/tmp/complete", "test", "", "")
	if err := stage.Run(t.Context(), job); err == nil {
		t.Errorf("Run with absolute script path expected error, got nil")
	}
}

func TestFinalizeStage_Name(t *testing.T) {
	t.Parallel()
	if got := (&FinalizeStage{}).Name(); got != "finalize" {
		t.Errorf("Name = %q; want %q", got, "finalize")
	}
}

func TestFinalizeStage_Run(t *testing.T) {
	t.Parallel()
	job, srcDir := stageJob(t)
	finalDir := t.TempDir()
	job.FinalDir = filepath.Join(finalDir, "FinalJobName")

	// Create a dummy file to move.
	dummyFile := "data.bin"
	if err := os.WriteFile(filepath.Join(srcDir, dummyFile), []byte("content"), 0o600); err != nil {
		t.Fatalf("write dummy file: %v", err)
	}

	if err := NewFinalizeStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify file is in new location and old location is gone.
	if _, err := os.Stat(filepath.Join(job.FinalDir, dummyFile)); err != nil {
		t.Errorf("file not found in final destination: %v", err)
	}
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Errorf("source directory still exists")
	}
}

// ---------- TestDeobfuscateStage_NoRenames ----------

// TestDeobfuscateStage_NoRenames verifies that deobfuscation is a no-op when
// files already have recognizable (non-obfuscated) names.
func TestDeobfuscateStage_NoRenames(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)

	// Create files with normal, recognizable names.
	normalFiles := []string{"movie.mkv", "info.nfo", "sample.jpg"}
	for _, name := range normalFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := NewDeobfuscateStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify all files still exist with their original names.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range normalFiles {
		if !got[want] {
			t.Errorf("file %q missing after deobfuscate; got %v", want, got)
		}
	}
	if len(entries) != len(normalFiles) {
		t.Errorf("file count = %d, want %d", len(entries), len(normalFiles))
	}
}

func TestPar2CleanupStage_NoFiles(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	// No par2 or backup files created in dir.
	stage := NewPar2CleanupStage(true)
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, line := range job.OutputLines {
		if strings.Contains(line, "Cleaned up ") {
			t.Errorf("unexpected cleanup log: %q", line)
		}
	}
}
