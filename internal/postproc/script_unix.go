//go:build unix

package postproc

import (
	"errors"
	"os/exec"
	"syscall"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// setProcessGroup configures cmd to start in its own process group and wires
// cmd.Cancel to terminate the entire process tree on context cancellation.
func setProcessGroup(cmd *exec.Cmd) {
	cmdutil.SetProcessGroup(cmd)
}

// killProcessGroup sends SIGKILL to the entire process group of cmd.
func killProcessGroup(cmd *exec.Cmd) {
	cmdutil.KillProcessGroup(cmd)
}

// isETXTBSY reports whether err wraps the ETXTBSY ("text file busy") errno.
// This transient error occurs when exec races with a recently-written file.
func isETXTBSY(err error) bool {
	return errors.Is(err, syscall.ETXTBSY)
}
