package cmdutil

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestBuildSandboxedCommand_Disabled(t *testing.T) {
	ctx := context.Background()
	cmd, err := BuildSandboxedCommand(ctx, CmdConfig{}, SandboxConfig{Enabled: false, TargetDir: "/tmp"}, "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.Args) < 2 || cmd.Args[len(cmd.Args)-1] != "hello" {
		t.Errorf("unexpected args: %v", cmd.Args)
	}
}

func TestBuildSandboxedCommand_EmptyTargetDir(t *testing.T) {
	ctx := context.Background()
	cmd, err := BuildSandboxedCommand(ctx, CmdConfig{}, SandboxConfig{Enabled: true, TargetDir: ""}, "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.Args) < 2 || cmd.Args[len(cmd.Args)-1] != "hello" {
		t.Errorf("unexpected args: %v", cmd.Args)
	}
}

func TestBuildSandboxedCommand_StrictFailure(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(file string) (string, error) {
		return "", errors.New("not found")
	}

	ctx := context.Background()
	cfg := SandboxConfig{Enabled: true, Strict: true, TargetDir: "/tmp"}
	_, err := BuildSandboxedCommand(ctx, CmdConfig{}, cfg, "echo", "hello")
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "freebsd" {
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

	ctx := context.Background()
	cfg := SandboxConfig{Enabled: true, Strict: false, TargetDir: "/tmp"}
	cmd, err := BuildSandboxedCommand(ctx, CmdConfig{Nice: "-n 10"}, cfg, "echo", "hello")
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
}
