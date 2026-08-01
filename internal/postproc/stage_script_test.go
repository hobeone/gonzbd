package postproc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestScriptStage_PathTraversalRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)

	tmpDir := t.TempDir()
	scriptDir := filepath.Join(tmpDir, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secretScript := filepath.Join(tmpDir, "secret.sh")
	writeScript(t, secretScript, []byte("#!/bin/sh\nexit 0\n"))

	job.Queue.Script = "../secret.sh"
	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")

	err := stage.Run(t.Context(), job)
	if err == nil {
		t.Fatalf("Run with path traversal ../secret.sh expected error, got nil")
	}
	if !strings.Contains(err.Error(), "traverses outside script_dir") {
		t.Errorf("expected traversal error message, got %v", err)
	}
}

func TestScriptStage_SymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)

	scriptDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideScript := filepath.Join(outsideDir, "evil.sh")
	writeScript(t, outsideScript, []byte("#!/bin/sh\nexit 0\n"))

	// Create a symlink inside scriptDir that points outside.
	if err := os.Symlink(outsideScript, filepath.Join(scriptDir, "hook.sh")); err != nil {
		t.Fatal(err)
	}

	job.Queue.Script = "hook.sh"
	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")

	err := stage.Run(t.Context(), job)
	if err == nil {
		t.Fatal("Run with symlink escaping script_dir expected error, got nil")
	}
	if !strings.Contains(err.Error(), "traverses outside script_dir") {
		t.Errorf("expected traversal error message, got %v", err)
	}
}

func TestScriptStage_AbsolutePathRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)

	absScript := filepath.Join(t.TempDir(), "abs.sh")
	writeScript(t, absScript, []byte("#!/bin/sh\nexit 0\n"))

	job.Queue.Script = absScript
	stage := NewScriptStage("/nonexistent-dir", "/tmp/complete", "test", "", "")

	err := stage.Run(t.Context(), job)
	if err == nil {
		t.Fatalf("Run with absolute script path expected error, got nil")
	}
	if !strings.Contains(err.Error(), "is absolute") {
		t.Errorf("expected absolute path error message, got %v", err)
	}
}

func TestScriptStage_EmptyScriptDirRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)

	job.Queue.Script = "sonarr.sh"
	stage := NewScriptStage("", "/tmp/complete", "test", "", "")

	err := stage.Run(t.Context(), job)
	if err == nil {
		t.Fatalf("Run with empty script_dir expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no script_dir is set") {
		t.Errorf("expected empty script_dir error message, got %v", err)
	}
}

func TestScriptStage_CaseInsensitiveNoneAndDefault(t *testing.T) {
	t.Parallel()
	for _, scriptName := range []string{"none", "NONE", "default", "DEFAULT", "Default"} {
		t.Run(scriptName, func(t *testing.T) {
			job, _ := stageJob(t)
			job.Queue.Script = scriptName
			stage := NewScriptStage("/nonexistent", "/tmp/complete", "test", "", "")
			if err := stage.Run(t.Context(), job); err != nil {
				t.Errorf("Run(%q) expected nil, got %v", scriptName, err)
			}
		})
	}
}

// TestScriptStage_CanFailFalse_SetsFailMsg verifies that when
// ScriptCanFail is false (default) and the script exits non-zero,
// job.FailMsg is set so buildSummaryEntry records Status="Failed".
// Without this, a failed script produces a "Completed" history entry.
func TestScriptStage_CanFailFalse_SetsFailMsg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "fail.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 7\n"))

	job.Queue.Script = "fail.sh"
	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")
	// ScriptCanFail defaults to false — do not call SetScriptCanFail.

	err := stage.Run(t.Context(), job)
	if err == nil {
		t.Fatal("expected non-nil error for non-zero script exit with ScriptCanFail=false")
	}

	// The critical assertion: FailMsg must be populated so that
	// buildSummaryEntry sees it and records Status="Failed".
	if job.FailMsg == "" {
		t.Error("job.FailMsg should be set when ScriptCanFail=false and script exits non-zero")
	}
	if !strings.Contains(job.FailMsg, "fail.sh") {
		t.Errorf("job.FailMsg should mention script name, got %q", job.FailMsg)
	}
}

// TestScriptStage_CanFailTrue_NoFailMsg verifies that when
// ScriptCanFail is true and the script exits non-zero, job.FailMsg
// is NOT set (the failure is swallowed as a warning).
func TestScriptStage_CanFailTrue_NoFailMsg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "fail.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 3\n"))

	job.Queue.Script = "fail.sh"
	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")
	stage.SetScriptCanFail(true)

	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("expected nil error with ScriptCanFail=true, got %v", err)
	}
	if job.FailMsg != "" {
		t.Errorf("job.FailMsg should be empty with ScriptCanFail=true, got %q", job.FailMsg)
	}
}

func TestScriptStage_ValidScriptAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()
	job, _ := stageJob(t)

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "sonarr.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 0\n"))

	job.Queue.Script = "sonarr.sh"
	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")

	if err := stage.Run(t.Context(), job); err != nil {
		t.Errorf("Run with valid script sonarr.sh expected nil, got %v", err)
	}
}
