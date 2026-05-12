// Package cmdutil provides command-execution utilities shared by tool
// wrappers (unrar, 7z, par2, scripts).
package cmdutil

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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

// BuildCommand constructs an *exec.Cmd with optional nice/ionice wrapping.
// The returned Cmd has not been started. The caller is responsible for
// setting Stdout, Stderr, Dir, Env, etc.
//
// When CmdConfig is empty, this is equivalent to exec.CommandContext.
// When Nice and/or Ionice are set, the actual binary and args are
// prepended with the priority wrapper(s).
//
// The wrapping order matches SABnzbd:
//
//	nice <nice_args> ionice <ionice_args> <name> <args...>
func BuildCommand(ctx context.Context, cfg CmdConfig, name string, args ...string) *exec.Cmd {
	if cfg.IsEmpty() {
		return exec.CommandContext(ctx, name, args...) //nolint:gosec // caller-supplied args
	}

	var fullArgs []string

	if cfg.Nice != "" {
		fullArgs = append(fullArgs, "nice")
		fullArgs = append(fullArgs, splitFields(cfg.Nice)...)
	}
	if cfg.Ionice != "" {
		fullArgs = append(fullArgs, "ionice")
		fullArgs = append(fullArgs, splitFields(cfg.Ionice)...)
	}

	fullArgs = append(fullArgs, name)
	fullArgs = append(fullArgs, args...)

	return exec.CommandContext(ctx, fullArgs[0], fullArgs[1:]...) //nolint:gosec // priority-wrapped command
}

// FormatCmdPrefix returns the nice/ionice prefix that BuildCommand would
// prepend, suitable for display in logs and UI. Returns empty string when
// no wrapping is configured.
func FormatCmdPrefix(cfg CmdConfig) string {
	if cfg.IsEmpty() {
		return ""
	}
	var parts []string
	if cfg.Nice != "" {
		parts = append(parts, "nice")
		parts = append(parts, splitFields(cfg.Nice)...)
	}
	if cfg.Ionice != "" {
		parts = append(parts, "ionice")
		parts = append(parts, splitFields(cfg.Ionice)...)
	}
	return strings.Join(parts, " ")
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

// splitFields splits s on whitespace, returning nil for empty strings.
func splitFields(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Fields(s)
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
