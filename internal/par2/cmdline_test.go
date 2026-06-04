package par2

import (
	"slices"
	"testing"
	"time"
)

// ---------- RepairWith / VerifyWith command-line construction ----------

// TestRepairWith_NoQuietFlag verifies that RepairWith does NOT add -q
// to the command line (I7: quiet mode suppresses File:/Target: lines
// needed by ParseRepairOutput).
func TestRepairWith_NoQuietFlag(t *testing.T) {
	t.Parallel()

	var captured string
	opts := RunOptions{
		Command: "/nonexistent/par2",
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	// RepairWith will fail to exec; we only care about the captured command.
	_, _ = RepairWith(t.Context(), opts, "/tmp/does-not-exist.par2")

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}
	// The command should contain "r" as the first arg (repair mode).
	// It must NOT contain -q.
	if containsArg(captured, "-q") {
		t.Errorf("repair command should NOT include -q: %q", captured)
	}
	// Verify mode should still include -q (tested separately).
	if !containsArg(captured, "r") {
		t.Errorf("repair command missing 'r' subcommand: %q", captured)
	}
}

// TestVerifyWith_HasQuietFlag verifies that VerifyWith includes -q
// (quiet mode is fine for verification since we only need aggregate status).
func TestVerifyWith_HasQuietFlag(t *testing.T) {
	t.Parallel()

	var captured string
	opts := RunOptions{
		Command: "/nonexistent/par2",
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	_, _ = VerifyWith(t.Context(), opts, "/tmp/does-not-exist.par2")

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}
	if !containsArg(captured, "-q") {
		t.Errorf("verify command should include -q: %q", captured)
	}
}

// TestRepairWith_ExtraArgs verifies ExtraArgs are appended to the repair command.
func TestRepairWith_ExtraArgs(t *testing.T) {
	t.Parallel()

	var captured string
	opts := RunOptions{
		Command:   "/nonexistent/par2",
		ExtraArgs: []string{"--memory=256m", "--threads=4"},
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	_, _ = RepairWith(t.Context(), opts, "/tmp/does-not-exist.par2")

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}
	for _, arg := range []string{"--memory=256m", "--threads=4"} {
		if !containsArg(captured, arg) {
			t.Errorf("ExtraArgs: cmdline missing %q: %q", arg, captured)
		}
	}
}

// TestVerifyWith_ExtraArgs verifies ExtraArgs are appended to the verify command.
func TestVerifyWith_ExtraArgs(t *testing.T) {
	t.Parallel()

	var captured string
	opts := RunOptions{
		Command:   "/nonexistent/par2",
		ExtraArgs: []string{"--threads=2"},
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	_, _ = VerifyWith(t.Context(), opts, "/tmp/does-not-exist.par2")

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}
	if !containsArg(captured, "--threads=2") {
		t.Errorf("ExtraArgs: cmdline missing --threads=2: %q", captured)
	}
}

// ---------- RepairOutput.ETA ----------

func TestRepairOutput_ETA_BeforeStart(t *testing.T) {
	t.Parallel()
	ro := &RepairOutput{
		RepairStarted: time.Time{}, // zero
		RepairPercent: 50,
	}
	if eta := ro.ETA(); eta != 0 {
		t.Errorf("ETA before start = %v, want 0", eta)
	}
}

func TestRepairOutput_ETA_ZeroPercent(t *testing.T) {
	t.Parallel()
	ro := &RepairOutput{
		RepairStarted: time.Now().Add(-10 * time.Second),
		RepairPercent: 0,
	}
	if eta := ro.ETA(); eta != 0 {
		t.Errorf("ETA at 0%% = %v, want 0", eta)
	}
}

func TestRepairOutput_ETA_Complete(t *testing.T) {
	t.Parallel()
	ro := &RepairOutput{
		RepairStarted: time.Now().Add(-10 * time.Second),
		RepairPercent: 100,
	}
	if eta := ro.ETA(); eta != 0 {
		t.Errorf("ETA at 100%% = %v, want 0", eta)
	}
}

func TestRepairOutput_ETA_InProgress(t *testing.T) {
	t.Parallel()
	ro := &RepairOutput{
		RepairStarted: time.Now().Add(-10 * time.Second),
		RepairPercent: 50,
	}
	eta := ro.ETA()
	// At 50%, with 10s elapsed, ETA should be ~10s (remaining=50/50=1.0 * 10s).
	if eta < 5*time.Second || eta > 15*time.Second {
		t.Errorf("ETA at 50%% with 10s elapsed = %v, want ~10s", eta)
	}
}

// ---------- RepairOutput.RepairStarted ----------

func TestParseRepairOutput_RepairStartedSet(t *testing.T) {
	t.Parallel()
	// When "Repair is possible" appears, RepairStarted should be set.
	output := `Verifying damaged files:
Target: "file1.dat" - Repair is possible.
Repair is possible (1 block(s) needed)
Repairing:  50.0%
Repair complete.
`
	before := time.Now()
	ro := ParseRepairOutput(output)
	after := time.Now()

	if ro.RepairStarted.IsZero() {
		t.Error("RepairStarted should be set when 'Repair is possible' is parsed")
	}
	// RepairStarted should be between before and after.
	if ro.RepairStarted.Before(before) || ro.RepairStarted.After(after) {
		t.Errorf("RepairStarted = %v, want between %v and %v",
			ro.RepairStarted, before, after)
	}
}

func TestParseRepairOutput_RepairStartedNotSet_AllOK(t *testing.T) {
	t.Parallel()
	output := "All files are correct, repair is not required.\n"
	ro := ParseRepairOutput(output)
	if !ro.RepairStarted.IsZero() {
		t.Error("RepairStarted should be zero when no repair is needed")
	}
}

// ---------- helpers ----------

// containsArg checks if arg appears as a whitespace-delimited token in cmdLine.
func containsArg(cmdLine, arg string) bool {
	return slices.Contains(splitFields(cmdLine), arg)
}

// splitFields is a simple whitespace split.
func splitFields(s string) []string {
	var fields []string
	for _, f := range splitOnWhitespace(s) {
		if f != "" {
			fields = append(fields, f)
		}
	}
	return fields
}

func splitOnWhitespace(s string) []string {
	var result []string
	start := -1
	for i, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if start >= 0 {
				result = append(result, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		result = append(result, s[start:])
	}
	return result
}

func TestFormatParCmdLineDirect(t *testing.T) {
	got := formatParCmdLine("/usr/bin/par2", []string{"r", "-q", "/tmp/file.par2"})
	want := "/usr/bin/par2 r -q /tmp/file.par2"
	if got != want {
		t.Errorf("formatParCmdLine = %q, want %q", got, want)
	}

	// Empty args
	got = formatParCmdLine("/usr/bin/par2", nil)
	want = "/usr/bin/par2"
	if got != want {
		t.Errorf("formatParCmdLine = %q, want %q", got, want)
	}
}
