//go:build linux

package postproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRunScript_ETXTBSY_RetriesThenReportsTheStartError holds the script open
// for writing for the whole run, so Linux refuses every exec of it with
// ETXTBSY. RunScript must retry rather than give up on the first refusal —
// the 1+2+4+8 ms backoff before its four retries puts a floor under the
// elapsed time — and then report the start failure with ETXTBSY still
// reachable through errors.Is.
func TestRunScript_ETXTBSY_RetriesThenReportsTheStartError(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "busy.sh")
	f, err := os.OpenFile(scriptPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // test-owned path in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.WriteString("#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatal(err)
	}

	result := RunScript(t.Context(), scriptPath, ScriptInput{FinalDir: dir})

	if !errors.Is(result.Err, syscall.ETXTBSY) {
		t.Fatalf("Err = %v, want one wrapping ETXTBSY", result.Err)
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a script that never started", result.ExitCode)
	}
	const backoffFloor = 15 * time.Millisecond // 1+2+4+8 ms before the 4 retries
	if result.Duration < backoffFloor {
		t.Errorf("Duration = %v, want >= %v: RunScript did not retry through its backoff", result.Duration, backoffFloor)
	}
}

// TestRunScript_CancelledBeforeStart verifies that a context already done
// when RunScript is called starts nothing and reports the context's error.
func TestRunScript_CancelledBeforeStart(t *testing.T) {
	skipIfNoShell(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	scriptPath := makeScript(t, dir, "mark.sh", "touch "+marker)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := RunScript(ctx, scriptPath, ScriptInput{FinalDir: dir})

	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", result.Err)
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("script ran despite the cancelled context (stat marker: %v)", err)
	}
}
