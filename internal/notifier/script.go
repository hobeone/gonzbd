package notifier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ScriptConfig holds settings for the script notification runner.
type ScriptConfig struct {
	// Path is the absolute path to the executable. No shell is invoked.
	Path      string
	Timeout   time.Duration
	EventMask []EventType
}

// ScriptNotifier runs a user-supplied executable for each accepted event.
// The event is passed as positional argv and also in SAB_* environment variables,
// mirroring the Python implementation's custom notification script contract.
type ScriptNotifier struct {
	cfg ScriptConfig
	run func(*exec.Cmd) error
}

// NewScriptNotifier creates a ScriptNotifier from cfg.
func NewScriptNotifier(cfg ScriptConfig) *ScriptNotifier {
	return &ScriptNotifier{
		cfg: cfg,
		run: func(cmd *exec.Cmd) error { return cmd.Run() },
	}
}

// Name returns the notifier identifier.
func (s *ScriptNotifier) Name() string { return "script" }

// Accepts reports whether this notifier is configured to handle t.
func (s *ScriptNotifier) Accepts(t EventType) bool {
	return acceptsAny(s.cfg.EventMask, t)
}

// Send executes the configured script with the event data as argv and env.
// argv: [script, event_type, title, body]
// env: inherits os.Environ() plus SAB_EVENT, SAB_TITLE, SAB_BODY, SAB_JOB_NAME.
func (s *ScriptNotifier) Send(ctx context.Context, e Event) error {
	timeout := s.cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := append(os.Environ(),
		"SAB_EVENT="+e.Type.String(),
		"SAB_TITLE="+e.Title,
		"SAB_BODY="+e.Body,
		"SAB_JOB_NAME="+e.JobName,
	)

	// Retry loop for transient ETXTBSY ("text file busy"). On Linux,
	// exec can fail with ETXTBSY when the script file was recently
	// written — the kernel may not have fully released the inode's
	// write reference yet. This is common after config reloads that
	// regenerate script files. Up to 3 retries with short backoff.
	const maxRetries = 3
	for attempt := range maxRetries {
		//nolint:gosec // G204: script path comes from admin config, not user input
		cmd := exec.CommandContext(ctx, s.cfg.Path, e.Type.String(), e.Title, e.Body)
		// WaitDelay ensures the process is force-killed if it doesn't exit
		// promptly after the context is cancelled, preventing hangs when a
		// script spawns child processes that outlive the shell.
		cmd.WaitDelay = 2 * time.Second
		cmd.Env = env

		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		err := s.run(cmd)
		if err == nil {
			return nil
		}

		// Check for ETXTBSY — retry if we haven't exhausted attempts.
		if attempt < maxRetries-1 && isETXTBSY(err) {
			select {
			case <-ctx.Done():
				return fmt.Errorf("script %s: %w", s.cfg.Path, ctx.Err())
			case <-time.After(time.Duration(attempt+1) * 5 * time.Millisecond):
				continue
			}
		}

		tail := strings.TrimSpace(stderr.String())
		if tail != "" {
			return fmt.Errorf("script %s: %w; stderr: %s", s.cfg.Path, err, tail)
		}
		return fmt.Errorf("script %s: %w", s.cfg.Path, err)
	}
	// Unreachable, but satisfies the compiler.
	return fmt.Errorf("script %s: exhausted retries", s.cfg.Path)
}

// isETXTBSY reports whether err (or any wrapped error) is ETXTBSY.
func isETXTBSY(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ETXTBSY)
}
