package postproc

import (
	"encoding/json"
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

// runFilesScript runs a script that writes $SAB_FILES to a file through the
// stage against j and returns the raw variable, plus the stage error.
func runFilesScript(t *testing.T, j *Job) (string, error) {
	t.Helper()
	scriptDir := t.TempDir()
	outFile := filepath.Join(scriptDir, "files.json")
	writeScript(t, filepath.Join(scriptDir, "files.sh"),
		fmt.Appendf(nil, "#!/bin/sh\nprintf '%%s' \"$SAB_FILES\" > %s\n", outFile))
	j.Script = "files.sh"
	err := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "").Run(t.Context(), j)
	data, readErr := os.ReadFile(outFile)
	if readErr != nil && err == nil {
		t.Fatalf("script wrote no output: %v", readErr)
	}
	return string(data), err
}

func TestScriptStage_SABFilesNestedSorted(t *testing.T) {
	t.Parallel()
	j, dir := stageJob(t)
	// "a.txt" must sort before "a/z.txt" ('.' < '/'), which is the reverse of
	// the order a directory walk visits them in, so only an explicit sort
	// over the full relative paths yields the want below.
	for _, rel := range []string{"b.mkv", "a/z.txt", "a.txt", "sub/deep/c.nfo", "sub/d.srt"} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory holding no file is not listed.
	if err := os.MkdirAll(filepath.Join(dir, "emptydir"), 0o750); err != nil {
		t.Fatal(err)
	}
	// A symlink is neither listed nor followed.
	if err := os.Symlink(filepath.Join(dir, "b.mkv"), filepath.Join(dir, "link.mkv")); err != nil {
		t.Fatal(err)
	}

	raw, err := runFilesScript(t, j)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("SAB_FILES %q is not a JSON array: %v", raw, err)
	}
	want := []string{"a.txt", "a/z.txt", "b.mkv", "sub/d.srt", "sub/deep/c.nfo"}
	if !slices.Equal(got, want) {
		t.Errorf("SAB_FILES = %v, want %v", got, want)
	}
}

func TestScriptStage_SABFilesEmptyDir(t *testing.T) {
	t.Parallel()
	j, _ := stageJob(t)
	raw, err := runFilesScript(t, j)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raw != "[]" {
		t.Errorf("SAB_FILES = %q, want %q", raw, "[]")
	}
}

func TestScriptStage_SABFilesWalkErrorFailsStage(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 directory, so no walk error can be provoked")
	}
	j, dir := stageJob(t)
	// The directory itself stays usable as the script's working directory, so
	// only the walk can be what fails the stage.
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) }) //nolint:errcheck // best-effort so TempDir cleanup can remove it
	scriptDir := t.TempDir()
	ran := filepath.Join(scriptDir, "ran")
	writeScript(t, filepath.Join(scriptDir, "files.sh"), fmt.Appendf(nil, "#!/bin/sh\ntouch %s\n", ran))
	j.Script = "files.sh"

	err := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "").Run(t.Context(), j)
	if err == nil {
		t.Fatal("expected the stage to report the walk error")
	}
	if _, statErr := os.Stat(ran); statErr == nil {
		t.Error("script ran despite the failed walk")
	}
}

func TestScriptStage_SABFilesTooLargeFailsStage(t *testing.T) {
	t.Parallel()
	j, dir := stageJob(t)
	sub := filepath.Join(dir, strings.Repeat("d", 200))
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	// 300 paths of ~400 bytes encode to over 100 KiB.
	for i := range 300 {
		name := fmt.Sprintf("%s%03d", strings.Repeat("n", 197), i)
		if err := os.WriteFile(filepath.Join(sub, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runFilesScript(t, j); err == nil {
		t.Fatal("expected an error when SAB_FILES would exceed the per-variable limit")
	}
}
