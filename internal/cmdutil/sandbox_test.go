package cmdutil

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// testLogger returns a logger and the buffer it writes to, so tests can
// assert on emitted log lines.
func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestBuildSandboxedCommand_Disabled(t *testing.T) {
	log, _ := testLogger()
	ctx := context.Background()
	cmd, err := BuildSandboxedCommand(ctx, log, CmdConfig{}, SandboxConfig{Enabled: false, TargetDir: "/tmp"}, "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.Args) < 2 || cmd.Args[len(cmd.Args)-1] != "hello" {
		t.Errorf("unexpected args: %v", cmd.Args)
	}
}

func TestBuildSandboxedCommand_EmptyTargetDir(t *testing.T) {
	log, _ := testLogger()
	ctx := context.Background()
	_, err := BuildSandboxedCommand(ctx, log, CmdConfig{}, SandboxConfig{Enabled: true, TargetDir: ""}, "echo", "hello")
	if !errors.Is(err, ErrSandboxMisconfigured) {
		t.Fatalf("expected ErrSandboxMisconfigured, got %v", err)
	}
}

func TestBuildSandboxedCommand_StrictFailure(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(file string) (string, error) {
		return "", errors.New("not found")
	}

	log, _ := testLogger()
	ctx := context.Background()
	cfg := SandboxConfig{Enabled: true, Strict: true, TargetDir: "/tmp"}
	_, err := BuildSandboxedCommand(ctx, log, CmdConfig{}, cfg, "echo", "hello")
	if runtime.GOOS == "linux" {
		if !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatalf("expected ErrSandboxUnavailable, got %v", err)
		}
	} else {
		if !errors.Is(err, ErrSandboxUnsupported) {
			t.Fatalf("expected ErrSandboxUnsupported, got %v", err)
		}
	}
}

func TestBuildSandboxedCommand_NonStrictFallback(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(file string) (string, error) {
		return "", errors.New("not found")
	}

	log, buf := testLogger()
	ctx := context.Background()
	cfg := SandboxConfig{Enabled: true, Strict: false, TargetDir: "/tmp"}
	cmd, err := BuildSandboxedCommand(ctx, log, CmdConfig{Nice: "-n 10"}, cfg, "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"nice", "-n", "10", "echo", "hello"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("got %d args %v, want %d args %v", len(cmd.Args), cmd.Args, len(want), want)
	}
	for i, w := range want {
		if cmd.Args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, cmd.Args[i], w)
		}
	}

	// Issue #97: the unsandboxed downgrade must not be silent — a warning
	// naming the command must be logged.
	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "unsandboxed") || !strings.Contains(logged, "echo") {
		t.Fatalf("expected a WARN log line about running %q unsandboxed, got: %q", "echo", logged)
	}
}

func TestBuildSandboxedCommand_SetsTMPDIR(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	log, _ := testLogger()
	ctx := context.Background()
	cfg := SandboxConfig{Enabled: true, Strict: false, TargetDir: "/sandbox/target"}
	cmd, err := BuildSandboxedCommand(ctx, log, CmdConfig{}, cfg, "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !slices.Contains(cmd.Env, "TMPDIR=/sandbox/target") {
		t.Fatalf("expected cmd.Env to contain TMPDIR=/sandbox/target when sandboxing enabled, got %v", cmd.Env)
	}
}

func TestBuildSandboxedCommand_MalformedPriorityArgs(t *testing.T) {
	log, _ := testLogger()
	ctx := context.Background()
	cfg := SandboxConfig{Enabled: false, TargetDir: "/tmp"}
	_, err := BuildSandboxedCommand(ctx, log, CmdConfig{Nice: "-n 15; rm -rf /"}, cfg, "echo", "hello")
	if err == nil {
		t.Error("expected error for malformed priority args when sandboxing disabled, got nil")
	}

	cfgEnabled := SandboxConfig{Enabled: true, TargetDir: "/tmp"}
	_, err = BuildSandboxedCommand(ctx, log, CmdConfig{Ionice: "-c2 | cat /etc/passwd"}, cfgEnabled, "echo", "hello")
	if err == nil {
		t.Error("expected error for malformed priority args when sandboxing enabled, got nil")
	}
}
