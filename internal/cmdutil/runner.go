// Package cmdutil provides command-execution utilities shared by tool
// wrappers (unrar, 7z, par2, scripts).
package cmdutil

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// CmdConfig controls process-priority wrapping for external commands.
// When Nice and/or Ionice are non-empty, the external command is
// prepended with the corresponding system utility, matching SABnzbd's
// build_and_run_command() behavior.
type CmdConfig struct {
	// Nice is the argument string for the nice(1) command.
	// Example: "-n 15" → the command runs as: nice -n 15 <cmd> <args...>
	// Empty disables nice wrapping.
	Nice string
	// Ionice is the argument string for the ionice(1) command.
	// Example: "-c2 -n4" → the command runs as: ionice -c2 -n4 <cmd> <args...>
	// When both Nice and Ionice are set, the order is:
	//   nice <nice_args> ionice <ionice_args> <cmd> <args...>
	// matching SABnzbd's priority_env wrapping order.
	// Empty disables ionice wrapping.
	Ionice string
}

// IsEmpty returns true when neither nice nor ionice is configured.
func (c CmdConfig) IsEmpty() bool {
	return c.Nice == "" && c.Ionice == ""
}

// BuildCommand constructs an *exec.Cmd with optional nice/ionice wrapping
// and configures process-group isolation via SetProcessGroup so context
// cancellation terminates the entire child process group.
// The returned Cmd has not been started. The caller is responsible for
// setting Stdout, Stderr, Dir, Env, etc.
//
// When CmdConfig is empty, this constructs exec.CommandContext(ctx, name, args...)
// with SetProcessGroup configured.
// When Nice and/or Ionice are set, the actual binary and args are
// prepended with the priority wrapper(s).
//
// The wrapping order matches SABnzbd:
//
//	nice <nice_args> ionice <ionice_args> <name> <args...>
func BuildCommand(ctx context.Context, cfg CmdConfig, name string, args ...string) (*exec.Cmd, error) {
	if cfg.IsEmpty() {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // caller-supplied args
		SetProcessGroup(cmd)
		return cmd, nil
	}

	fullArgs, err := buildPriorityArgs(cfg)
	if err != nil {
		return nil, err
	}
	fullArgs = append(fullArgs, name)
	fullArgs = append(fullArgs, args...)

	cmd := exec.CommandContext(ctx, fullArgs[0], fullArgs[1:]...) //nolint:gosec // priority-wrapped command
	SetProcessGroup(cmd)
	return cmd, nil
}

// SetProcessGroup configures cmd to start in its own Unix process group
// (Setpgid: true, preserving any existing non-nil SysProcAttr). When cmd was
// constructed with exec.CommandContext (so cmd.Cancel is non-nil), it replaces
// cmd.Cancel with KillProcessGroup so context cancellation terminates the
// entire process tree rather than only the direct child. It also sets
// cmd.WaitDelay to 3 seconds if not already set, preventing cmd.Wait from
// hanging indefinitely when background processes inherit standard descriptors.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			KillProcessGroup(cmd)
			return nil
		}
	}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = 3 * time.Second
	}
}

// KillProcessGroup sends SIGKILL to the process group of cmd (-pid) and falls
// back to killing the direct process if the group signal fails. If cmd has not
// been started, it is a no-op.
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		//nolint:errcheck // best-effort fallback when process is not a group leader or already exited
		_ = cmd.Process.Kill()
	}
}

// FormatCmdPrefix returns the nice/ionice prefix that BuildCommand would
// prepend, suitable for display in logs and UI. Returns empty string when
// no wrapping is configured or when the priority arguments are malformed.
func FormatCmdPrefix(cfg CmdConfig) string {
	if cfg.IsEmpty() {
		return ""
	}
	args, err := buildPriorityArgs(cfg)
	if err != nil {
		return ""
	}
	return strings.Join(args, " ")
}

// buildPriorityArgs returns the nice/ionice prefix arguments for the
// given config. Used by both BuildCommand and FormatCmdPrefix.
// Malformed argument strings containing shell metacharacters return an error
// so that BuildCommand can abort process execution on malformed priority strings.
func buildPriorityArgs(cfg CmdConfig) ([]string, error) {
	var parts []string
	if cfg.Nice != "" {
		if err := ValidatePriorityArgs("nice", cfg.Nice); err != nil {
			return nil, err
		}
		parts = append(parts, "nice")
		parts = append(parts, strings.Fields(cfg.Nice)...)
	}
	if cfg.Ionice != "" {
		if err := ValidatePriorityArgs("ionice", cfg.Ionice); err != nil {
			return nil, err
		}
		parts = append(parts, "ionice")
		parts = append(parts, strings.Fields(cfg.Ionice)...)
	}
	return parts, nil
}

// ValidatePriorityArgs checks that a nice or ionice argument string
// contains only safe characters (alphanumerics, hyphens, spaces, and
// digits). Returns an error if shell metacharacters are present.
func ValidatePriorityArgs(kind, args string) error {
	if args == "" {
		return nil
	}
	for _, r := range args {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == ' ', r == '\t':
			// safe
		default:
			return fmt.Errorf("invalid character %q in %s args %q: only alphanumerics, hyphens, and spaces are allowed", r, kind, args)
		}
	}
	return nil
}

// ParseExtraParams splits a whitespace-delimited parameter string into
// individual args. Each arg must start with '-' (validated). Returns nil
// for empty input. This validates user-supplied extra_unrar_params and
// extra_par2_params config values.
func ParseExtraParams(params string) ([]string, error) {
	if strings.TrimSpace(params) == "" {
		return nil, nil
	}
	args := strings.Fields(params)
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return nil, fmt.Errorf("extra parameter %q does not start with '-'", a)
		}
	}
	return args, nil
}

// unrarAllowedPrefixes defines the parameter prefixes SABnzbd considers
// safe for user-specified unrar arguments. Only flags matching one of these
// prefixes are permitted.
//
//   - "-mlp"  — use large memory pages
//   - "-om"   — ignore/set symlinks (e.g. -om-)
//   - "-ri"   — recovery volume priority and thread count (e.g. -ri10:5)
var unrarAllowedPrefixes = []string{"-mlp", "-om", "-ri"}

// ValidateUnrarParams checks that each argument in args is in SABnzbd's
// unrar parameter allowlist. Returns an error describing the first
// disallowed flag. This prevents users from passing flags that could
// cause data loss (e.g. -df for delete after extract) or security issues.
func ValidateUnrarParams(args []string) error {
	for _, a := range args {
		allowed := false
		for _, prefix := range unrarAllowedPrefixes {
			if strings.HasPrefix(a, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("unrar parameter %q is not allowed; permitted prefixes: %v", a, unrarAllowedPrefixes)
		}
	}
	return nil
}
