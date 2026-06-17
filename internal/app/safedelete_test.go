package app

import (
	"os"
	"path/filepath"
	"testing"
)

// mkdir creates dir (and parents) under t.TempDir-rooted paths, failing the test on error.
func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestSafeDeleteDir(t *testing.T) {
	t.Run("deletes target inside base", func(t *testing.T) {
		base := t.TempDir()
		target := mkdir(t, filepath.Join(base, "job"))
		if err := safeDeleteDir(target, base); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		if exists(target) {
			t.Fatal("target should have been deleted")
		}
	})

	t.Run("refuses and does not delete a target outside base", func(t *testing.T) {
		root := t.TempDir()
		base := mkdir(t, filepath.Join(root, "base"))
		outside := mkdir(t, filepath.Join(root, "outside"))
		marker := filepath.Join(outside, "keep.txt")
		if err := os.WriteFile(marker, []byte("KEEP"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := safeDeleteDir(outside, base)
		if err == nil {
			t.Fatal("expected containment-breach error, got nil")
		}
		if !exists(marker) {
			t.Fatal("SECURITY: outside dir was deleted despite being outside base")
		}
	})

	t.Run("refuses to delete a base itself", func(t *testing.T) {
		base := t.TempDir()
		if err := safeDeleteDir(base, base); err == nil {
			t.Fatal("expected refusal to delete base, got nil")
		}
		if !exists(base) {
			t.Fatal("base must not be deleted")
		}
	})

	t.Run("empty target is a no-op", func(t *testing.T) {
		if err := safeDeleteDir("", t.TempDir()); err != nil {
			t.Fatalf("expected nil no-op, got %v", err)
		}
	})

	t.Run("no non-empty base is a no-op and does not delete", func(t *testing.T) {
		target := mkdir(t, filepath.Join(t.TempDir(), "job"))
		if err := safeDeleteDir(target, "", ""); err != nil {
			t.Fatalf("expected nil no-op, got %v", err)
		}
		if !exists(target) {
			t.Fatal("target must not be deleted when no base is supplied")
		}
	})

	t.Run("deletes when target is within any of several bases", func(t *testing.T) {
		completeDir := t.TempDir()
		downloadDir := t.TempDir()
		target := mkdir(t, filepath.Join(downloadDir, "failed-job"))
		// target is under downloadDir (the second base), not completeDir.
		if err := safeDeleteDir(target, completeDir, downloadDir); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		if exists(target) {
			t.Fatal("target under the second base should have been deleted")
		}
	})

	// Regression for the os.Root hardening: a target path that is *lexically*
	// inside base but reaches outside through a symlinked component must NOT be
	// deleted. The previous lexical (filepath.Rel/PathWithin + os.RemoveAll)
	// implementation would have followed the symlink and deleted the victim.
	t.Run("refuses to delete through a symlinked component (os.Root)", func(t *testing.T) {
		base := t.TempDir()
		outside := t.TempDir()
		victim := filepath.Join(outside, "victim.txt")
		if err := os.WriteFile(victim, []byte("VICTIM"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Attacker-planted symlink inside base pointing out of the tree.
		if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
			t.Fatal(err)
		}
		// Lexically "base/link/victim.txt" is within base, but it resolves
		// outside via the symlink.
		target := filepath.Join(base, "link", "victim.txt")
		if err := safeDeleteDir(target, base); err == nil {
			t.Fatal("expected an error: deletion must not traverse the symlink out of base")
		}
		if !exists(victim) {
			t.Fatal("SECURITY: deleted a file outside base by traversing a symlinked component")
		}
	})
}
