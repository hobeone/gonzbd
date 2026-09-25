package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
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

// TestJobIDIsPathSafe_AcceptsOnlyASinglePathElement pins the predicate every
// manifest path goes through, case by case.
func TestJobIDIsPathSafe_AcceptsOnlyASinglePathElement(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]bool{
		"0123456789abcdef": true,
		"":                 false,
		".":                false,
		"..":               false,
		"a/b":              false,
		`a\b`:              false,
		"/abs":             false,
	} {
		if got := jobIDIsPathSafe(id); got != want {
			t.Errorf("jobIDIsPathSafe(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestManifestName_EndsInTheManifestSuffix pins the name the startup sweep
// strips back to a job ID: the two agree only while both use manifestSuffix.
func TestManifestName_EndsInTheManifestSuffix(t *testing.T) {
	t.Parallel()
	name, err := manifestName("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if name != "0123456789abcdef"+manifestSuffix {
		t.Errorf("manifestName = %q, want the ID followed by %q", name, manifestSuffix)
	}
	if _, err := manifestName("../x"); err == nil {
		t.Error("manifestName accepted a traversal")
	}
}

// TestRemoveManifestIn_RemovesOneManifestAndRefusesAnUnsafeID pins the
// confined delete reclaim uses.
func TestRemoveManifestIn_RemovesOneManifestAndRefusesAnUnsafeID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "0123456789abcdef"+manifestSuffix)
	if err := os.WriteFile(path, []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeManifestIn(dir, "0123456789abcdef"); err != nil {
		t.Fatalf("removeManifestIn: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the manifest survived its removal (stat: %v)", err)
	}
	if err := removeManifestIn(dir, "0123456789abcdef"); !os.IsNotExist(err) {
		t.Errorf("removing a missing manifest = %v, want a not-exist error the callers can ignore", err)
	}
	if err := removeManifestIn(dir, ".."); err == nil {
		t.Error("removeManifestIn accepted an unsafe ID")
	}
}

// writeJobManifestFixture builds a job whose manifest writeJobManifest can
// persist. jobID is passed through so a test can hand it an ID the path guard
// must refuse.
func writeJobManifestFixture(t *testing.T, jobID string) *job.Job {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "a.bin",
		Bytes:    1,
		Articles: []nzb.Article{{ID: "a@t", Bytes: 1, Number: 1}},
	}}}
	j, _, err := BuildIngestJob(cfg, parsed, "x.nzb",
		types.FetchOptions{NzbName: "x", JobID: jobID}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j
}

// TestWriteJobManifest_WritesTheFileTheHydratorReads covers the success path.
// Without this file a tick that hydrates the job settles it Failed, which is
// permanent — see the function's own doc.
func TestWriteJobManifest_WritesTheFileTheHydratorReads(t *testing.T) {
	admin := t.TempDir()
	j := writeJobManifestFixture(t, "")

	if err := writeJobManifest(admin, j); err != nil {
		t.Fatalf("writeJobManifest: %v", err)
	}
	path, err := manifestPath(admin, j.ID())
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if st.Size() == 0 {
		t.Error("the manifest is empty, so a hydrate would fail on it as surely as on a missing one")
	}
}

// TestWriteJobManifest_ReportsADirectoryItCannotCreate pins that the mkdir
// failure is returned rather than swallowed: a caller that admitted the job
// anyway would leave one no tick can hydrate.
func TestWriteJobManifest_ReportsADirectoryItCannotCreate(t *testing.T) {
	// A regular file as the parent makes MkdirAll fail with ENOTDIR, which
	// needs no permission changes and so behaves the same for root.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := writeJobManifest(filepath.Join(blocker, "admin"), writeJobManifestFixture(t, ""))
	if err == nil {
		t.Fatal("writeJobManifest reported success for an admin dir it could not create")
	}
	if !strings.Contains(err.Error(), "mkdir manifests") {
		t.Errorf("err = %v, want it to name the mkdir that failed", err)
	}
}

// TestWriteJobManifest_RefusesAnUnsafeJobID pins that the path guard is reached
// through this writer too, not only through manifestPath's direct callers.
func TestWriteJobManifest_RefusesAnUnsafeJobID(t *testing.T) {
	admin := t.TempDir()
	j := writeJobManifestFixture(t, "../evil")

	err := writeJobManifest(admin, j)
	if err == nil {
		t.Fatal("writeJobManifest accepted a job ID that escapes the manifest directory")
	}
	if !strings.Contains(err.Error(), "unsafe job ID") {
		t.Errorf("err = %v, want it to name the unsafe ID", err)
	}
}
