package postproc

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestScriptStage_PathTraversalRejected(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)

	tmpDir := t.TempDir()
	scriptDir := filepath.Join(tmpDir, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secretScript := filepath.Join(tmpDir, "secret.sh")
	writeScript(t, secretScript, []byte("#!/bin/sh\nexit 0\n"))

	job.Script = "../secret.sh"
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

	job.Script = "hook.sh"
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
	t.Parallel()
	job, _ := stageJob(t)

	absScript := filepath.Join(t.TempDir(), "abs.sh")
	writeScript(t, absScript, []byte("#!/bin/sh\nexit 0\n"))

	job.Script = absScript
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
	t.Parallel()
	job, _ := stageJob(t)

	job.Script = "sonarr.sh"
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
			job.Script = scriptName
			stage := NewScriptStage("/nonexistent", "/tmp/complete", "test", "", "")
			if err := stage.Run(t.Context(), job); err != nil {
				t.Errorf("Run(%q) expected nil, got %v", scriptName, err)
			}
		})
	}
}

// TestScriptStage_CanFailFalse_NoFailMsg verifies that when
// ScriptCanFail is false (default) and the script exits non-zero,
// the output is recorded and Run returns nil without setting job.FailMsg,
// so the job still completes (matching SABnzbd's cfg.script_can_fail()).
func TestScriptStage_CanFailFalse_NoFailMsg(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "fail.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\necho \"minor script warning\"\nexit 7\n"))

	job.Script = "fail.sh"
	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")
	// ScriptCanFail defaults to false — do not call SetScriptCanFail.

	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("expected nil error for non-zero script exit with ScriptCanFail=false, got %v", err)
	}
	if job.FailMsg != "" {
		t.Errorf("job.FailMsg should be empty with ScriptCanFail=false, got %q", job.FailMsg)
	}
	if !slices.ContainsFunc(job.OutputLines, func(line string) bool {
		return strings.Contains(line, "minor script warning")
	}) {
		t.Errorf("expected script output in job.OutputLines, got %v", job.OutputLines)
	}
}

// TestScriptStage_CanFailTrue_SetsFailMsg verifies that when
// ScriptCanFail is true and the script exits non-zero, job.FailMsg
// is set and Run returns an error so buildSummaryEntry records Status="Failed".
func TestScriptStage_CanFailTrue_SetsFailMsg(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "fail.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 3\n"))

	job.Script = "fail.sh"
	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")
	stage.SetScriptCanFail(true)

	err := stage.Run(t.Context(), job)
	if err == nil {
		t.Fatal("expected non-nil error for non-zero script exit with ScriptCanFail=true")
	}
	if job.FailMsg == "" {
		t.Error("job.FailMsg should be set when ScriptCanFail=true and script exits non-zero")
	}
	if !strings.Contains(job.FailMsg, "fail.sh") {
		t.Errorf("job.FailMsg should mention script name, got %q", job.FailMsg)
	}
}

// TestScriptStage_CanFailTrue_PreservesPriorFailure verifies that when an
// earlier stage has already failed (job.FailMsg != "", job.ParError, or
// job.UnpackError), a non-zero script exit with ScriptCanFail=true does not
// overwrite job.FailMsg and mask the original stage failure.
func TestScriptStage_CanFailTrue_PreservesPriorFailure(t *testing.T) {
	t.Parallel()
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "fail.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 3\n"))

	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")
	stage.SetScriptCanFail(true)

	for _, tc := range []struct {
		name        string
		setup       func(*Job)
		wantFailMsg string
	}{
		{
			name:        "prior FailMsg preserved",
			setup:       func(j *Job) { j.FailMsg = "unwanted extension detected" },
			wantFailMsg: "unwanted extension detected",
		},
		{
			name:        "prior ParError keeps empty FailMsg",
			setup:       func(j *Job) { j.ParError = true },
			wantFailMsg: "",
		},
		{
			name:        "prior UnpackError keeps empty FailMsg",
			setup:       func(j *Job) { j.UnpackError = true },
			wantFailMsg: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j, _ := stageJob(t)
			j.Script = "fail.sh"
			tc.setup(j)

			if err := stage.Run(t.Context(), j); err == nil {
				t.Fatal("expected non-nil error for non-zero script exit with ScriptCanFail=true")
			}
			if j.FailMsg != tc.wantFailMsg {
				t.Errorf("job.FailMsg = %q, want %q", j.FailMsg, tc.wantFailMsg)
			}
		})
	}
}

// TestScriptStage_BytesExcludesDiscardedPar2 pins the fix for a regression
// this task's immutable TotalBytes() introduced: a discarded (FetchNever)
// recovery volume never reaches disk, but it was still counted into
// SAB_BYTES because ScriptInput.Bytes read job.Queue.TotalBytes(), which no
// longer shrinks across a discard. The correct figure is ExpectedBytes,
// which excludes any file that is not FetchAlways — matching what
// buildHistoryEntry already reports for the same job (see
// internal/app/history_helper.go).
func TestScriptStage_BytesExcludesDiscardedPar2(t *testing.T) {
	t.Parallel()

	j := job.New("bytes-test", "bytes-test.nzb", job.Policy{})
	m := job.NewManifest([]job.JobFile{
		{Subject: `"movie.mkv" yEnc`, Bytes: 1000, Articles: []job.JobArticle{{ID: "c@x", Bytes: 1000}}},
		{Subject: `"movie.par2" yEnc`, Bytes: 50, Articles: []job.JobArticle{{ID: "i@x", Bytes: 50}}},
		{Subject: `"movie.vol000+01.par2" yEnc`, Bytes: 500, Articles: []job.JobArticle{{ID: "v@x", Bytes: 500}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := j.SetFileFetchPolicy(2, job.FetchIfNeeded); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}
	if !j.Progress().HasDeferredPar2() {
		t.Fatal("fixture guard: expected a deferred recovery volume")
	}
	if err := j.SetFileFetchPolicy(2, job.FetchNever); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}

	wantExpected := j.Progress().ExpectedBytes()
	wantTotal := m.TotalBytes()
	if wantExpected >= wantTotal {
		t.Fatalf("fixture guard: ExpectedBytes (%d) must be less than TotalBytes (%d), or this test cannot distinguish them", wantExpected, wantTotal)
	}

	job := &Job{Job: j, DownloadDir: t.TempDir()}

	scriptDir := t.TempDir()
	outFile := filepath.Join(scriptDir, "bytes.txt")
	scriptPath := filepath.Join(scriptDir, "report.sh")
	writeScript(t, scriptPath, fmt.Appendf(nil, "#!/bin/sh\necho \"$SAB_BYTES\" > %s\n", outFile))

	job.Script = "report.sh"
	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading script output: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != fmt.Sprintf("%d", wantExpected) {
		t.Errorf("SAB_BYTES = %q, want %d (ExpectedBytes, excluding the discarded recovery volume); TotalBytes is %d",
			got, wantExpected, wantTotal)
	}
}

func TestScriptStage_ValidScriptAllowed(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "sonarr.sh")
	writeScript(t, scriptPath, []byte("#!/bin/sh\nexit 0\n"))

	job.Script = "sonarr.sh"
	stage := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "")

	if err := stage.Run(t.Context(), job); err != nil {
		t.Errorf("Run with valid script sonarr.sh expected nil, got %v", err)
	}
}
