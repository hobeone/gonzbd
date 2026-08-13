//go:build crash

package crash

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

// TestMain builds the daemon once for the whole package.
//
// The suite is Linux-only and says so by failing rather than skipping: every
// claim it makes rests on SIGKILL semantics and on posix_fadvise, and a
// silent skip on another platform would report a green suite that ran
// nothing.
func TestMain(m *testing.M) {
	if runtime.GOOS != "linux" {
		fmt.Fprintf(os.Stderr, "test/crash requires Linux (SIGKILL + posix_fadvise); GOOS=%s\n", runtime.GOOS)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "gonzbd-crash-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "make a build directory: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	binPath, err = buildBinary(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
