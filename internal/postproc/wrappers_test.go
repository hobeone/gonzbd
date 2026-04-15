package postproc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// stageJob builds a *Job with a fresh tmp DownloadDir and a minimal
// queue.Job. Tests that need files in the DownloadDir can add them via
// the returned path.
func stageJob(t *testing.T) (*Job, string) {
	t.Helper()
	dir := t.TempDir()
	return &Job{
		Queue: &queue.Job{
			ID:       "test-job-id",
			Name:     "test.job",
			Filename: "test.nzb",
		},
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

func TestSortStage_Name(t *testing.T) {
	t.Parallel()
	if got := (&SortStage{}).Name(); got != "sort" {
		t.Errorf("Name = %q; want %q", got, "sort")
	}
}

func TestSortStage_NoRules(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	destRoot := t.TempDir()
	if err := NewSortStage(nil, destRoot).Run(t.Context(), job); err != nil {
		t.Fatalf("Run with no rules: %v", err)
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
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: test-only script needs exec bit
		t.Fatalf("write script: %v", err)
	}

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
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil { //nolint:gosec // G306: test-only script needs exec bit
		t.Fatalf("write script: %v", err)
	}

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
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil { //nolint:gosec // G306: test-only script needs exec bit
		t.Fatalf("write: %v", err)
	}

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

	// Script field is an absolute path; ScriptDir (intentionally bogus)
	// must not be prepended.
	scriptPath := filepath.Join(t.TempDir(), "abs.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: test-only script needs exec bit
		t.Fatalf("write: %v", err)
	}
	job.Queue.Script = scriptPath

	stage := NewScriptStage("/nonexistent-dir", "/tmp/complete", "test", "", "")
	if err := stage.Run(t.Context(), job); err != nil {
		t.Errorf("Run with absolute script path: %v", err)
	}
}
