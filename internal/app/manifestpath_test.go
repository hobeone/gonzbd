package app

import (
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
