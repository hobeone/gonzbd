package notifier

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
}

// NewScriptNotifier creates a ScriptNotifier from cfg.
func NewScriptNotifier(cfg ScriptConfig) *ScriptNotifier {
	return &ScriptNotifier{cfg: cfg}
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

	//nolint:gosec // G204: script path comes from admin config, not user input
	cmd := exec.CommandContext(ctx, s.cfg.Path, e.Type.String(), e.Title, e.Body)
	// WaitDelay ensures the process is force-killed if it doesn't exit
	// promptly after the context is cancelled, preventing hangs when a
	// script spawns child processes that outlive the shell.
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(),
		"SAB_EVENT="+e.Type.String(),
		"SAB_TITLE="+e.Title,
		"SAB_BODY="+e.Body,
		"SAB_JOB_NAME="+e.JobName,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if tail != "" {
			return fmt.Errorf("script %s: %w; stderr: %s", s.cfg.Path, err, tail)
		}
		return fmt.Errorf("script %s: %w", s.cfg.Path, err)
	}
	return nil
}
