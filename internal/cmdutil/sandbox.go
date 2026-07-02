package cmdutil

import (
	"context"
	"errors"
	"os/exec"
)

var (
	// ErrSandboxUnavailable indicates that OS sandboxing enforcement failed because the platform utility is missing or failed.
	ErrSandboxUnavailable = errors.New("strict sandbox enforcement failed: OS sandboxing utility unavailable or failed")
	// ErrSandboxUnsupported indicates that OS sandboxing is not supported on the target architecture.
	ErrSandboxUnsupported = errors.New("strict sandbox enforcement failed: OS sandboxing unsupported on this platform")
)

// lookPath is variable so tests can override binary lookup.
var lookPath = exec.LookPath

// SandboxConfig specifies the directory containment boundaries and strictness.
type SandboxConfig struct {
	// TargetDir is the absolute path to the directory where write operations are permitted.
	TargetDir string
	// Enabled determines if OS-level sandboxing should be attempted.
	Enabled bool
	// Strict determines if execution must abort when sandboxing cannot be established.
	Strict bool
}

// BuildSandboxedCommand constructs an *exec.Cmd wrapped with OS-level directory containment
// followed by nice/ionice process priorities.
func BuildSandboxedCommand(ctx context.Context, priorityCfg CmdConfig, sandboxCfg SandboxConfig, name string, args ...string) (*exec.Cmd, error) {
	if !sandboxCfg.Enabled || sandboxCfg.TargetDir == "" {
		return BuildCommand(ctx, priorityCfg, name, args...), nil
	}

	sName, sArgs, err := wrapSandbox(sandboxCfg.TargetDir, name, args)
	if err != nil {
		if sandboxCfg.Strict {
			return nil, err
		}
		return BuildCommand(ctx, priorityCfg, name, args...), nil
	}
	return BuildCommand(ctx, priorityCfg, sName, sArgs...), nil
}
