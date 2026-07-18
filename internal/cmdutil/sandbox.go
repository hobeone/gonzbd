package cmdutil

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
)

var (
	// ErrSandboxUnavailable indicates that OS sandboxing enforcement failed because the platform utility is missing or failed.
	ErrSandboxUnavailable = errors.New("strict sandbox enforcement failed: OS sandboxing utility unavailable or failed")
	// ErrSandboxUnsupported indicates that OS sandboxing is not supported on the target architecture.
	ErrSandboxUnsupported = errors.New("strict sandbox enforcement failed: OS sandboxing unsupported on this platform")
	// ErrSandboxMisconfigured indicates SandboxConfig.Enabled is true but no
	// TargetDir was provided. This is a caller wiring bug, not a runtime
	// sandbox failure — it must not be silently read as "sandboxing off".
	ErrSandboxMisconfigured = errors.New("sandbox enabled but no target directory configured")
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
// followed by nice/ionice process priorities. log receives a warning whenever
// sandboxing was requested but the resulting command runs unsandboxed
// (non-strict fallback) — this degradation is otherwise invisible to
// operators. See issue #97.
func BuildSandboxedCommand(ctx context.Context, log *slog.Logger, priorityCfg CmdConfig, sandboxCfg SandboxConfig, name string, args ...string) (*exec.Cmd, error) {
	if !sandboxCfg.Enabled {
		return BuildCommand(ctx, priorityCfg, name, args...)
	}
	if sandboxCfg.TargetDir == "" {
		return nil, ErrSandboxMisconfigured
	}

	sName, sArgs, err := wrapSandbox(sandboxCfg.TargetDir, name, args)
	if err != nil {
		if sandboxCfg.Strict {
			return nil, err
		}
		log.Warn("sandbox unavailable, running extractor unsandboxed",
			"command", name, "error", err)
		return BuildCommand(ctx, priorityCfg, name, args...)
	}
	cmd, err := BuildCommand(ctx, priorityCfg, sName, sArgs...)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Environ(), "TMPDIR="+sandboxCfg.TargetDir)
	return cmd, nil
}
