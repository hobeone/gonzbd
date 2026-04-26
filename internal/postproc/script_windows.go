//go:build windows

package postproc

import "os/exec"

// setProcessGroup is a no-op on Windows; process group semantics differ.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup kills only the direct process on Windows.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		//nolint:errcheck // best-effort kill
		_ = cmd.Process.Kill()
	}
}

// isETXTBSY always returns false on Windows; ETXTBSY is a Unix-only errno.
func isETXTBSY(_ error) bool { return false }
