package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
)

// manifestDir returns the directory holding one gzipped JSON manifest per job.
//
// It and the three helpers below are the only code that knows this layout. Six
// sites built the same filepath.Join independently before they were folded
// here, which is the "derived value with more than one computer" smell
// AGENTS.md's Standing Design Rule 2 names: each copy had to re-derive, on its
// own, whether the job ID reaching it was safe to put in a path.
func manifestDir(adminDir string) string {
	return filepath.Join(adminDir, "queue", "manifests")
}

// writeJobManifest persists j's manifest under adminDir, creating the manifest
// directory if it is absent. A job with no manifest writes nothing and is not
// an error.
//
// Persisting it is what lets the job survive an eviction. A tick hydrates any
// job that holds resources but is not resident, and a manifest that will not
// read settles the job Failed — permanently, because Outcome is write-once
// (reconcileResidency, internal/dispatch/tick.go).
func writeJobManifest(adminDir string, j *job.Job) error {
	if err := os.MkdirAll(manifestDir(adminDir), 0o750); err != nil {
		return fmt.Errorf("mkdir manifests: %w", err)
	}
	// Written only when the job has a manifest to write, and a job that cannot
	// produce one is not an error here — the shape AddJob has always had.
	if m, mErr := j.Manifest(); mErr == nil && m != nil {
		data, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal manifest: %w", err)
		}
		mpath, err := manifestPath(adminDir, j.ID())
		if err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}
		if err := fsutil.WriteGzAtomicBytes(mpath, data); err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}
	}
	return nil
}

// manifestName returns the file name — not a path — for a job's manifest, or an
// error if jobID cannot safely be used as a single path element.
//
// The daemon mints every job ID itself: newJobID (ingest.go) returns 16
// lowercase hex characters from crypto/rand, and it is the fallback whenever
// types.FetchOptions.JobID is empty. `git grep -n 'types\.FetchOptions{' --
// internal cmd ':!*_test.go'` finds 5 construction sites; of those, only
// rebuildJobFromNZB sets JobID, and it passes a history entry's own NzoID.
// But the ID arrives here from the API without re-validation —
// /api?mode=queue&name=delete&value=<csv> reaches RemoveJob unchanged — so it
// is untrusted at this boundary.
//
// Standing Design Rule 1's carve-out is the reason this guard stays rather than
// being argued away: a value interpolated into a path keeps its check, and the
// check says why where it sits.
func manifestName(jobID string) (string, error) {
	if !jobIDIsPathSafe(jobID) {
		return "", fmt.Errorf("app: manifest: unsafe job ID %q", jobID)
	}
	return jobID + manifestSuffix, nil
}

// manifestSuffix ends every manifest's file name after its job ID.
const manifestSuffix = ".json.gz"

// manifestPath returns the absolute path of one job's manifest.
//
// Its only production caller is writeJobManifest — pinned by
// TestManifestPath_HasOneProductionCaller.
// export_test.go's ManifestPath also hands it to a test that needs to name the
// file without repeating this layout. Writes go through fsutil.WriteGzAtomic,
// whose temp-file → fsync → close → rename sequence is what
// docs/durability-contract.md relies on; reimplementing that inside an os.Root
// would trade a durability guarantee for a confinement one the write path does
// not need, since it is reached with a job the daemon has just constructed
// rather than with an ID off the wire.
//
// Reads and deletes do take their ID from outside, so they use openManifestIn
// and removeManifestIn instead, which confine at the syscall rather than by
// inspecting a string.
func manifestPath(adminDir, jobID string) (string, error) {
	name, err := manifestName(jobID)
	if err != nil {
		return "", err
	}
	return filepath.Join(manifestDir(adminDir), name), nil
}

// removeManifestIn deletes a job's manifest from dir.
//
// os.Root is what makes the confinement structural: Root.Remove resolves the
// name against an open directory handle and refuses to traverse out of it, so
// a name that somehow got past manifestName still could not reach another
// directory. The string guard stays as well — it turns a bad ID into a named
// error rather than a filesystem call that happens to fail.
func removeManifestIn(dir, jobID string) error {
	name, err := manifestName(jobID)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.Remove(name)
}

// openManifestIn opens a job's manifest in dir for reading, confined the same
// way as removeManifestIn. Closing the returned file is the caller's job:
// closing the Root does not invalidate a file already opened through it.
func openManifestIn(dir, jobID string) (*os.File, error) {
	name, err := manifestName(jobID)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.Open(name)
}

// jobIDIsPathSafe reports whether id is a single path element that cannot
// escape the directory it is joined to: non-empty, carrying no separator, and
// not a relative marker. filepath.IsLocal additionally rejects absolute paths,
// paths that escape their parent, and the Windows reserved device names.
func jobIDIsPathSafe(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	return filepath.IsLocal(id)
}
