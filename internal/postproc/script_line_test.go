package postproc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScriptStage_RecordsScriptLogAndLine pins what the script stage records
// for history (#908): the captured output as the log, and a one-line summary
// in SABnzbd's format — the last non-blank line, "Exit(N): <line>" after a
// non-zero exit, "Script exit code is N" when that exit printed nothing,
// "Ran <script>" when a clean run printed nothing, and "Exit(-1): Cannot run
// script <script>" when it could not start.
func TestScriptStage_RecordsScriptLogAndLine(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 200)
	cases := []struct {
		name     string
		body     string
		mode     os.FileMode
		wantLine string
		wantLog  string
	}{
		{"last non-blank line", "echo first\necho '  last line  '\necho\necho '   '\n", 0o755, "last line", "first\n"},
		{"clean run with no output", "exit 0\n", 0o755, "Ran post.sh", ""},
		{"non-zero exit", "echo oops\nexit 3\n", 0o755, "Exit(3): oops", "oops\n"},
		{"non-zero exit with no output", "exit 3\n", 0o755, "Script exit code is 3", ""},
		{"script that cannot start", "exit 0\n", 0o644, "Exit(-1): Cannot run script post.sh", ""},
		{"long line", "echo " + long + "\n", 0o755, strings.Repeat("a", 147) + "...", long},
		{"markup", "echo '<b>done</b>'\n", 0o755, "done", "<b>done</b>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			job, _ := stageJob(t)
			scriptDir := t.TempDir()
			path := filepath.Join(scriptDir, "post.sh")
			writeScript(t, path, []byte("#!/bin/sh\n"+c.body))
			if err := os.Chmod(path, c.mode); err != nil {
				t.Fatal(err)
			}
			job.Script = "post.sh"

			_ = NewScriptStage(scriptDir, "/tmp/complete", "test", "", "").Run(t.Context(), job) //nolint:errcheck // the recorded output is what is asserted

			if got := job.ScriptLine(); got != c.wantLine {
				t.Errorf("ScriptLine() = %q, want %q", got, c.wantLine)
			}
			if !strings.Contains(job.ScriptLog(), c.wantLog) {
				t.Errorf("ScriptLog() = %q, want it to contain %q", job.ScriptLog(), c.wantLog)
			}
		})
	}
}

// TestScriptStage_NoScriptRecordsNothing pins that a job with no script, or
// one whose script was refused before it ran, files no script output.
func TestScriptStage_NoScriptRecordsNothing(t *testing.T) {
	t.Parallel()
	for _, script := range []string{"", "none", "../escape.sh"} {
		job, _ := stageJob(t)
		job.Script = script
		_ = NewScriptStage(t.TempDir(), "/tmp/complete", "test", "", "").Run(t.Context(), job) //nolint:errcheck // the recorded output is what is asserted
		if job.ScriptLine() != "" || job.ScriptLog() != "" {
			t.Errorf("script %q: ScriptLine() = %q, ScriptLog() = %q, want both empty", script, job.ScriptLine(), job.ScriptLog())
		}
	}
}

// TestFormatScriptLine covers each branch of the summary directly, including
// a non-zero exit with no exit status (a signal), which the stage tests above
// cannot produce reliably.
func TestFormatScriptLine(t *testing.T) {
	t.Parallel()
	nonZero := fmt.Errorf("%w (exit code 3): exit status 3", ErrNonZeroExit)
	cases := []struct {
		name string
		res  ScriptResult
		want string
	}{
		{"clean with output", ScriptResult{LastLine: "done"}, "done"},
		{"clean without output", ScriptResult{}, "Ran post.sh"},
		{"non-zero with output", ScriptResult{ExitCode: 3, LastLine: "oops", Err: nonZero}, "Exit(3): oops"},
		{"non-zero without output", ScriptResult{ExitCode: 3, Err: nonZero}, "Script exit code is 3"},
		{"killed by a signal", ScriptResult{ExitCode: -1, LastLine: "half", Err: fmt.Errorf("%w (exit code -1): signal: killed", ErrNonZeroExit)}, "Exit(-1): half"},
		{"could not start", ScriptResult{ExitCode: -1, Err: errors.New("RunScript: failed to start")}, "Exit(-1): Cannot run script post.sh"},
	}
	for _, c := range cases {
		if got := formatScriptLine("post.sh", c.res); got != c.want {
			t.Errorf("%s: formatScriptLine = %q, want %q", c.name, got, c.want)
		}
	}
}
