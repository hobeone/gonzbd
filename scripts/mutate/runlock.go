package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// runLockName is the lock file's name, both in the git dir and in the
// fallback location.
const runLockName = "gonzbd-mutate.lock"

// runLockPath is where a working tree's run lock lives: the path
// `git rev-parse --git-path gonzbd-mutate.lock` names when run in root. That
// is inside the tree's own git dir, which is per worktree (so sibling
// worktrees do not block each other, and scripts/run_tests.sh's
// one-worktree-per-worker sweep is not serialized), outside the work tree's
// status, and independent of TMPDIR. A root that is not a git checkout
// falls back to a file inside root, which is where a test's t.TempDir()
// cleans it up.
func runLockPath(root string) string {
	fallback := filepath.Join(root, runLockName)
	cmd := exec.Command("git", "rev-parse", "--git-path", runLockName)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return fallback
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return fallback
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return p
}

// acquireRunLock takes an exclusive, non-blocking flock for root's working
// tree and returns the function that releases it. A mutation run rewrites
// source files in place, so two runs in one tree can each build the other's
// mutation, or record the other's mutated bytes as their "original" (#812).
//
// The kernel drops an flock when the process dies, so a hard-killed run
// leaves nothing stale to clean up. O_NOFOLLOW refuses a lock path that has
// been replaced by a symlink.
func acquireRunLock(root string) (release func(), err error) {
	path := runLockPath(root)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // G304: path is git's own --git-path answer for root, or a fixed name inside root
	if err != nil {
		return nil, fmt.Errorf("open run lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // G115: fd fits in int
		holder := "unknown pid"
		if b, rerr := os.ReadFile(path); rerr == nil && strings.TrimSpace(string(b)) != "" { //nolint:gosec // G304: same path as above
			holder = "pid " + strings.TrimSpace(string(b))
		}
		_ = f.Close() //nolint:errcheck // the flock was refused, so nothing is held; the refusal is returned
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another mutate run is active in this working tree (%s), holding %s (%s); "+
				"two runs would mutate each other's source, so wait for it to finish", root, path, holder)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Best effort: the pid only decorates the refusal message.
	_ = f.Truncate(0)                          //nolint:errcheck // best effort: the pid only decorates the refusal message
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid()) //nolint:errcheck // best effort: the pid only decorates the refusal message
	return func() { _ = f.Close() }, nil       //nolint:errcheck // closing drops the flock, which the kernel also drops at exit
}
