package postproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// skipIfNoShell skips the test when /bin/sh is not available (e.g. Windows).
func skipIfNoShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script tests not supported on Windows")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("skipping: /bin/sh not available: %v", err)
	}
}

// makeScript writes a shell script with the given body to dir and returns its path.
// The script is marked executable.
func makeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("makeScript: %v", err)
	}
	return path
}

// fullInput returns a ScriptInput with all fields populated for test assertions.
func fullInput(t *testing.T) ScriptInput {
	t.Helper()
	return ScriptInput{
		FinalDir:        "/fake/final",
		CompleteDir:     "/fake/complete",
		NZBName:         "my.show.S01E01.nzb",
		JobName:         "My Show S01E01",
		ReportName:      "report-123",
		Category:        "tv",
		Group:           "alt.binaries.hdtv",
		Status:          0,
		FailureURL:      "https://example.com/fail",
		PPFlags:         3,
		ScriptName:      "myscript.sh",
		NZOID:           "nzo-abc123",
		DownloadTime:    420,
		URL:             "https://source.example.com/nzb",
		Version:         "5.6.0",
		APIKey:          "secret-key",
		APIURL:          "http://localhost:8080/api",
		Bytes:           1073741824,
		BytesDownloaded: 1000000000,
		AvgBPS:          2000000,
	}
}

// TestRunScript_PositionalArgs verifies that every positional argv matches the
// expected value at its index, mirroring Python's external_processing argv order:
//
//	script <complete_dir> <nzb_name> <job_name> <report_name> <category> <group> <status> <failure_url>
func TestRunScript_PositionalArgs(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()
	outFile := filepath.Join(dir, "args.txt")
	// Write all positional args ($1 through $8) to a file, one per line.
	scriptPath := makeScript(t, dir, "args.sh", fmt.Sprintf(`printf '%%s\n' "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" > %s`, outFile))

	in := fullInput(t)
	// Use t.TempDir() as FinalDir so the script cwd works.
	in.FinalDir = dir

	result := RunScript(t.Context(), scriptPath, in)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	want := []string{
		in.CompleteDir,
		in.NZBName,
		in.JobName,
		in.ReportName,
		in.Category,
		in.Group,
		fmt.Sprintf("%d", in.Status),
		in.FailureURL,
	}
	if len(lines) != len(want) {
		t.Fatalf("want %d args, got %d: %q", len(want), len(lines), lines)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("arg[%d]: want %q, got %q", i+1, w, lines[i])
		}
	}
}

// TestRunScript_EnvVars verifies all expected SAB_* environment variables are
// set with the correct values.
func TestRunScript_EnvVars(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")
	scriptPath := makeScript(t, dir, "env.sh", fmt.Sprintf(`env | grep '^SAB_' | sort > %s`, outFile))

	in := fullInput(t)
	in.FinalDir = dir

	result := RunScript(t.Context(), scriptPath, in)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading env output: %v", err)
	}
	envOutput := string(data)

	// Build expected key=value pairs from buildEnvMap for the sab-specific vars.
	m := buildEnvMap(in)

	// Required keys we always want to verify explicitly.
	required := []struct {
		key  string
		want string
	}{
		// From ENV_NZO_FIELDS:
		{"SAB_FILENAME", in.NZBName},
		{"SAB_FINAL_NAME", in.JobName},
		{"SAB_CAT", in.Category},
		{"SAB_GROUP", in.Group},
		{"SAB_NZO_ID", in.NZOID},
		{"SAB_PP", fmt.Sprintf("%d", in.PPFlags)},
		{"SAB_SCRIPT", in.ScriptName},
		{"SAB_STATUS", fmt.Sprintf("%d", in.Status)},
		{"SAB_URL", in.URL},
		// From extra_env_fields:
		{"SAB_COMPLETE_DIR", in.CompleteDir},
		{"SAB_COMPLETE_DIR", m["SAB_COMPLETE_DIR"]},
		{"SAB_PP_STATUS", fmt.Sprintf("%d", in.Status)},
		{"SAB_DOWNLOAD_TIME", fmt.Sprintf("%d", in.DownloadTime)},
		{"SAB_FAILURE_URL", in.FailureURL},
		// From create_env always-present:
		{"SAB_VERSION", in.Version},
		{"SAB_API_KEY", in.APIKey},
		{"SAB_API_URL", in.APIURL},
		// Go extensions:
		{"SAB_FINAL_PROCESSING_DIR", in.FinalDir},
		{"SAB_NZB_NAME", in.NZBName},
		{"SAB_REPORTNAME", in.ReportName},
	}

	for _, r := range required {
		needle := r.key + "=" + r.want
		if !strings.Contains(envOutput, needle) {
			t.Errorf("env var not found or wrong value:\n  want: %s\n  in output:\n%s", needle, envOutput)
		}
	}
}

// TestRunScript_ExitCodes verifies exit code handling and ErrNonZeroExit wrapping.
func TestRunScript_ExitCodes(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()

	cases := []struct {
		exitCode    int
		wantNonZero bool
	}{
		{0, false},
		{1, true},
		{42, true},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("exit%d", tc.exitCode), func(t *testing.T) {
			scriptPath := makeScript(t, dir, fmt.Sprintf("exit%d.sh", tc.exitCode),
				fmt.Sprintf("exit %d", tc.exitCode))
			result := RunScript(t.Context(), scriptPath, ScriptInput{FinalDir: dir})
			if result.ExitCode != tc.exitCode {
				t.Errorf("ExitCode: want %d, got %d", tc.exitCode, result.ExitCode)
			}
			isNonZero := errors.Is(result.Err, ErrNonZeroExit)
			if isNonZero != tc.wantNonZero {
				t.Errorf("errors.Is(ErrNonZeroExit): want %v, got %v (Err=%v)", tc.wantNonZero, isNonZero, result.Err)
			}
		})
	}
}

// TestRunScript_LogCapture verifies that both stdout and stderr are captured.
func TestRunScript_LogCapture(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()
	scriptPath := makeScript(t, dir, "log.sh", `echo "stdout-line"
echo "stderr-line" >&2`)

	result := RunScript(t.Context(), scriptPath, ScriptInput{FinalDir: dir})
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if !strings.Contains(result.LogBody, "stdout-line") {
		t.Errorf("stdout not captured; LogBody=%q", result.LogBody)
	}
	if !strings.Contains(result.LogBody, "stderr-line") {
		t.Errorf("stderr not captured; LogBody=%q", result.LogBody)
	}
}

// TestRunScript_LastLine verifies that LastLine is the last non-empty line.
func TestRunScript_LastLine(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()
	// printf to avoid trailing newline issues; echo adds one.
	scriptPath := makeScript(t, dir, "lastline.sh", `printf 'line1\nline2\n\n\n'`)

	result := RunScript(t.Context(), scriptPath, ScriptInput{FinalDir: dir})
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.LastLine != "line2" {
		t.Errorf("LastLine: want %q, got %q", "line2", result.LastLine)
	}
}

// TestRunScript_LogCap verifies that LogBody is bounded by MaxLogBytes and ends
// on a line boundary when truncated.
func TestRunScript_LogCap(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()
	// Write ~1 MiB (well beyond 512 KiB) of lines. Each line is "x" * 99 + newline = 100 bytes.
	// 10000 such lines = 1,000,000 bytes > MaxLogBytes (524288).
	scriptPath := makeScript(t, dir, "biglog.sh",
		`i=0; while [ $i -lt 10000 ]; do printf '%0.s-' {1..99}; echo; i=$((i+1)); done`)

	result := RunScript(t.Context(), scriptPath, ScriptInput{FinalDir: dir})
	// Non-zero exit is fine; we care about the log cap.

	if len(result.LogBody) > MaxLogBytes {
		t.Errorf("LogBody too large: %d > %d", len(result.LogBody), MaxLogBytes)
	}
	if len(result.LogBody) == 0 {
		t.Errorf("LogBody is empty — script may not have run")
	}
	// LogBody must end on a line boundary (last byte is '\n' or log is empty).
	if len(result.LogBody) > 0 && result.LogBody[len(result.LogBody)-1] != '\n' {
		t.Errorf("LogBody does not end on a line boundary; last byte: %q", result.LogBody[len(result.LogBody)-1])
	}
}

// TestRunScript_ContextCancel verifies that a running script is killed when the
// context is cancelled, and RunScript returns well before the script would finish.
func TestRunScript_ContextCancel(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()
	scriptPath := makeScript(t, dir, "sleep.sh", "sleep 30")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := RunScript(ctx, scriptPath, ScriptInput{FinalDir: dir})
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("RunScript took too long (%v) after context cancellation", elapsed)
	}
	if result.Err == nil {
		t.Error("expected non-nil Err after context cancellation")
	}
}

// TestRunScript_MissingScript verifies that a missing or empty script path returns
// a non-nil Err without panicking.
func TestRunScript_MissingScript(t *testing.T) {
	t.Run("empty path", func(t *testing.T) {
		result := RunScript(t.Context(), "", ScriptInput{})
		if result.Err == nil {
			t.Error("expected non-nil Err for empty scriptPath")
		}
	})
	t.Run("no such binary", func(t *testing.T) {
		result := RunScript(t.Context(), "/no/such/binary/ever", ScriptInput{})
		if result.Err == nil {
			t.Error("expected non-nil Err for non-existent binary")
		}
	})
}

// TestRunScript_Cwd verifies that the script's working directory is set to FinalDir.
func TestRunScript_Cwd(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()
	outFile := filepath.Join(dir, "cwd.txt")
	scriptPath := makeScript(t, dir, "cwd.sh", fmt.Sprintf(`pwd > %s`, outFile))

	result := RunScript(t.Context(), scriptPath, ScriptInput{FinalDir: dir})
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading cwd output: %v", err)
	}
	got := strings.TrimRight(string(data), "\n\r")

	// On macOS /tmp may be a symlink; resolve both.
	gotResolved, _ := filepath.EvalSymlinks(got)
	dirResolved, _ := filepath.EvalSymlinks(dir)
	if gotResolved != dirResolved && got != dir {
		t.Errorf("cwd: want %q (or %q), got %q (or %q)", dir, dirResolved, got, gotResolved)
	}
}
