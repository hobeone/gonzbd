//go:build !windows

package postproc

import (
	"os/exec"
	"syscall"
)

// setProcessGroup configures cmd to start in its own process group so that
// when the process is killed the entire process tree (including grandchildren
// spawned by shell scripts) receives the signal.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGKILL to the entire process group of cmd.
// If the process has already exited, the error is silently ignored.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Negative pid targets the process group.
	//nolint:errcheck // best-effort kill; process may have already exited
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
