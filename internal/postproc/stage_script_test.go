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
