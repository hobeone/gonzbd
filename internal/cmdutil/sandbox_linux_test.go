//go:build linux

package cmdutil

import (
	"errors"
	"slices"
	"testing"
)

// TestWrapSandbox_Linux_IncludesProcAndDev pins the bwrap invocation so that a
// fresh /proc and /dev are always mounted. Without them, --ro-bind "/" "/"
// leaves those submounts empty and dynamically-linked extractors can fail.
func TestWrapSandbox_Linux_IncludesProcAndDev(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) { return "/usr/bin/bwrap", nil }

	bin, args, err := wrapSandbox("/downloads/job", "unrar", []string{"e", "-p-", "a.rar", "/downloads/job"})
	if err != nil {
		t.Fatalf("wrapSandbox: %v", err)
	}
	if bin != "bwrap" {
		t.Errorf("bin = %q, want bwrap", bin)
	}

	// A fresh proc and dev must be mounted, and both must appear before the
	// "--" argument separator (i.e. they are bwrap options, not tool args).
	sep := slices.Index(args, "--")
	if sep < 0 {
		t.Fatalf("missing -- separator in args: %v", args)
	}
	opts := args[:sep]
	for _, want := range [][2]string{{"--proc", "/proc"}, {"--dev", "/dev"}} {
		i := slices.Index(opts, want[0])
		if i < 0 || i+1 >= len(opts) || opts[i+1] != want[1] {
			t.Errorf("bwrap options missing %s %s; got: %v", want[0], want[1], opts)
		}
	}
	// The read-only root bind and the writable target bind must still be present.
	if slices.Index(opts, "--ro-bind") < 0 || slices.Index(opts, "--bind") < 0 {
		t.Errorf("bwrap options missing root/target binds: %v", opts)
	}

	// The tool and its args must follow the separator unchanged.
	want := []string{"unrar", "e", "-p-", "a.rar", "/downloads/job"}
	if !slices.Equal(args[sep+1:], want) {
		t.Errorf("tool args after -- = %v, want %v", args[sep+1:], want)
	}
}

// TestWrapSandbox_Linux_BwrapMissing verifies the unavailable-sandbox error
// path is preserved.
func TestWrapSandbox_Linux_BwrapMissing(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) { return "", errors.New("not found") }

	if _, _, err := wrapSandbox("/downloads/job", "unrar", nil); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("expected ErrSandboxUnavailable, got %v", err)
	}
}
