package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestManifestPath_RejectsUnsafeJobID pins the guard that keeps an API-supplied
// job ID from escaping the manifest directory. CodeQL's go/path-injection
// traces /api?mode=queue&name=delete&value=<csv> to RemoveJob's filepath.Join;
// registry membership makes that unreachable today, but this is the check that
// says so locally instead of five hops away in the ingest path.
func TestManifestPath_RejectsUnsafeJobID(t *testing.T) {
	for _, id := range []string{
		"",
		".",
		"..",
		"../evil",
		"../../etc/passwd",
		"a/b",
		`a\b`,
		"/absolute",
	} {
		t.Run(id, func(t *testing.T) {
			got, err := manifestPath("/admin", id)
			if err == nil {
				t.Fatalf("manifestPath(%q) = %q, nil; want an error — an unsafe ID reached the filesystem", id, got)
			}
			if got != "" {
				t.Errorf("manifestPath(%q) returned path %q alongside its error; want empty", id, got)
			}
		})
	}
}

// TestManifestPath_AcceptsMintedIDs pins that the guard does not reject the IDs
// the daemon actually mints — newJobID returns 16 lowercase hex characters —
// nor the shorter identifiers this package's own tests use.
func TestManifestPath_AcceptsMintedIDs(t *testing.T) {
	for _, id := range []string{"0123456789abcdef", "j1", "job-1", "job_1"} {
		got, err := manifestPath("/admin", id)
		if err != nil {
			t.Fatalf("manifestPath(%q) = %v; want no error", id, err)
		}
		want := filepath.Join("/admin", "queue", "manifests", id+".json.gz")
		if got != want {
			t.Errorf("manifestPath(%q) = %q, want %q", id, got, want)
		}
		if !strings.HasSuffix(got, ".json.gz") {
			t.Errorf("manifestPath(%q) = %q, want a .json.gz suffix", id, got)
		}
	}
}

// TestOpenManifestIn_RefusesSymlinkEscapingTheDirectory pins the property the
// string guard cannot provide. jobIDIsPathSafe inspects the ID; it says nothing
// about what the resulting name resolves to on disk. os.Root resolves the name
// against an open directory handle and refuses to leave it, so a manifest entry
// that is a symlink out of the directory does not hand back the target.
//
// Plain os.Open follows that symlink, which is what makes this a real
// difference rather than a restatement of the guard.
func TestOpenManifestIn_RefusesSymlinkEscapingTheDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "manifests")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(base, "secret.txt")
	if err := os.WriteFile(secret, []byte("not a manifest"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(dir, "escape.json.gz")
	if err := os.Symlink(filepath.Join("..", "secret.txt"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Baseline: the path-based open this replaced would have followed it.
	if f, err := os.Open(link); err == nil { //nolint:gosec // fixture path, test-only
		_ = f.Close()
	} else {
		t.Fatalf("setup: os.Open(%q) = %v; the fixture does not demonstrate the difference", link, err)
	}

	f, err := openManifestIn(dir, "escape")
	if err == nil {
		_ = f.Close()
		t.Fatal("openManifestIn followed a symlink out of the manifest directory")
	}
}
