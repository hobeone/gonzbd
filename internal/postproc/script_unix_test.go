//go:build unix

package postproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestSetProcessGroup_ConfiguresPgid verifies that setProcessGroup arms
// cmd.SysProcAttr.Setpgid, which is what makes killProcessGroup's
// negative-pid signal reach the whole process tree rather than just cmd
// itself.
func TestSetProcessGroup_ConfiguresPgid(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("true")
	setProcessGroup(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil after setProcessGroup")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid = false, want true")
	}
}

// TestSetProcessGroup_PreservesExistingSysProcAttr verifies that a
// caller-supplied SysProcAttr is reused rather than replaced, so any
// other field the caller already set survives.
func TestSetProcessGroup_PreservesExistingSysProcAttr(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("true")
	existing := &syscall.SysProcAttr{}
	cmd.SysProcAttr = existing
	setProcessGroup(cmd)

	if cmd.SysProcAttr != existing {
		t.Error("setProcessGroup replaced an existing non-nil SysProcAttr instead of reusing it")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid = false, want true")
	}
}

// TestKillProcessGroup_KillsRunningProcess pins the actual killing
// behavior: a process started under setProcessGroup must die when
// killProcessGroup is called, not merely have its pid signaled and
// ignored.
func TestKillProcessGroup_KillsRunningProcess(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sleep", "30")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	killProcessGroup(cmd)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Wait returned nil error for a killed process, want a signal-kill error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit within 5s of killProcessGroup")
	}
}

// TestKillProcessGroup_NilProcess verifies the nil-Process guard: calling
// killProcessGroup on a cmd that was never started must not panic.
func TestKillProcessGroup_NilProcess(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("true")
	killProcessGroup(cmd) // must not panic
}

// TestIsETXTBSY verifies isETXTBSY recognizes a wrapped ETXTBSY errno and
// rejects both a different errno and a nil error.
func TestIsETXTBSY(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare ETXTBSY", syscall.ETXTBSY, true},
		{"wrapped ETXTBSY", fmt.Errorf("exec: %w", syscall.ETXTBSY), true},
		{"different errno", syscall.ENOENT, false},
		{"unrelated error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isETXTBSY(tc.err); got != tc.want {
				t.Errorf("isETXTBSY(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRunScript_SetsidGrandchildWaitDelayBindsExecution verifies that when a
// script spawns a detached grandchild in a new session (setsid) that inherits
// stdout and sleeps, RunScript still terminates within WaitDelay + margin after
// context cancellation instead of blocking indefinitely waiting for pipe EOF.
func TestRunScript_SetsidGrandchildWaitDelayBindsExecution(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "setsid_child.sh")
	pidFile := filepath.Join(dir, "grandchild.pid")
	scriptContent := fmt.Sprintf(`#!/bin/sh
setsid sh -c 'echo $$ > %q; exec sleep 30' &
echo "grandchild started"
wait
`, pidFile)
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			var pid int
			if _, scanErr := fmt.Sscanf(string(data), "%d", &pid); scanErr == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := RunScript(ctx, scriptPath, ScriptInput{FinalDir: dir})
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("RunScript blocked for %v (expected ~3s bounded by WaitDelay, not 30s grandchild sleep)", elapsed)
	}
	if result.Err == nil {
		t.Fatal("RunScript expected timeout/cancellation error, got nil")
	}
}
