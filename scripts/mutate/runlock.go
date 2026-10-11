package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// runLockPath is where a working tree's run lock lives. It is keyed on the
// tree's root path (repoRoot is `git rev-parse --show-toplevel`, which is
// per worktree), so sibling worktrees get different locks and
// scripts/run_tests.sh's one-worktree-per-worker sweep is not serialized.
// It sits in the temp dir rather than in the tree so a mutation run never
// dirties the tree it is protecting, and so a root that is not a git
// checkout still works.
func runLockPath(root string) string {
	key := root
	if abs, err := filepath.Abs(root); err == nil {
		key = abs
	}
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = resolved
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(os.TempDir(), "gonzbd-mutate-"+hex.EncodeToString(sum[:8])+".lock")
}

// acquireRunLock takes an exclusive, non-blocking flock for root's working
// tree and returns the function that releases it. A mutation run rewrites
// source files in place, so two runs in one tree can each build the other's
// mutation, or record the other's mutated bytes as their "original" (#812).
//
// The kernel drops an flock when the process dies, so a hard-killed run
// leaves nothing stale to clean up.
func acquireRunLock(root string) (release func(), err error) {
	path := runLockPath(root)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: path is derived from a hash of the repo root under os.TempDir
	if err != nil {
		return nil, fmt.Errorf("open run lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // G115: fd fits in int
		holder := "unknown pid"
		if b, rerr := os.ReadFile(path); rerr == nil && strings.TrimSpace(string(b)) != "" { //nolint:gosec // G304: same path as above
			holder = "pid " + strings.TrimSpace(string(b))
		}
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another mutate run is active in this working tree (%s), holding %s (%s); "+
				"two runs would mutate each other's source, so wait for it to finish", root, path, holder)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Best effort: the pid only decorates the refusal message.
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() { _ = f.Close() }, nil
}
