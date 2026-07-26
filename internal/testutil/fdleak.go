// Package testutil provides automated test assertions and utilities for GoNZBD.
package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// takeFDSnapshot returns a map of open file descriptor numbers (e.g., "3", "4")
// to their resolved symlink target paths on Linux (/proc/self/fd). On non-Linux
// operating systems, it returns an empty map.
func takeFDSnapshot() map[string]string {
	snap := make(map[string]string)
	if runtime.GOOS != "linux" {
		return snap
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return snap
	}
	for _, e := range entries {
		target, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err == nil {
			snap[e.Name()] = target
		}
	}
	return snap
}

// AssertNoFDLeaks takes a baseline snapshot of open file descriptors on Linux
// and registers a test cleanup handler. When the test completes, it compares
// current file descriptors against the baseline. If targetDirs are provided,
// it flags any new open file descriptors whose resolved paths fall inside any
// of the targetDirs. If no targetDirs are specified, it flags any newly opened
// regular file or directory path (excluding special filesystems like /dev/,
// /proc/, /sys/, /run/, sockets, and pipes).
func AssertNoFDLeaks(t testing.TB, targetDirs ...string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	before := takeFDSnapshot()

	t.Cleanup(func() {
		after := takeFDSnapshot()
		var leaks []string
		for fd, target := range after {
			if _, existed := before[fd]; existed {
				continue
			}
			if len(targetDirs) > 0 {
				for _, dir := range targetDirs {
					cleanDir := filepath.Clean(dir)
					if strings.HasPrefix(target, cleanDir) {
						leaks = append(leaks, fmt.Sprintf("fd %s -> %q", fd, target))
						break
					}
				}
			} else if isRegularOrDirPath(target) {
				leaks = append(leaks, fmt.Sprintf("fd %s -> %q", fd, target))
			}
		}
		if len(leaks) > 0 {
			t.Errorf("file descriptor leak detected (%d unclosed FDs):\n%s", len(leaks), strings.Join(leaks, "\n"))
		}
	})
}

// isRegularOrDirPath returns true if path looks like a standard filesystem
// file or directory path, filtering out Linux kernel pseudo-files, sockets,
// pipes, and eventfds that naturally open/close during background goroutine
// and test runner activity.
func isRegularOrDirPath(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	if strings.HasPrefix(path, "/dev/") ||
		strings.HasPrefix(path, "/proc/") ||
		strings.HasPrefix(path, "/sys/") ||
		strings.HasPrefix(path, "/run/") {
		return false
	}
	return true
}
