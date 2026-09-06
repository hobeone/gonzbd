package app

import (
	"fmt"
	"path/filepath"
	"strings"
)

// manifestDir returns the directory holding one gzipped JSON manifest per job.
//
// It and manifestPath are the only two functions that know this layout. Six
// sites built the same filepath.Join independently before they were folded
// here, which is the "derived value with more than one computer" smell
// AGENTS.md's Standing Design Rule 2 names: each copy had to re-derive, on its
// own, whether the job ID reaching it was safe to put in a path.
func manifestDir(adminDir string) string {
	return filepath.Join(adminDir, "queue", "manifests")
}

// manifestPath returns the on-disk path of one job's manifest, or an error if
// jobID cannot safely be used as a path element.
//
// It is the sole gatekeeper for a job ID reaching the filesystem, and the guard
// is stated here rather than left to be re-derived from the ingest path.
//
// The daemon mints every job ID itself — newJobID (ingest.go) returns 16
// lowercase hex characters from crypto/rand, and types.FetchOptions.JobID is
// set at exactly one non-test site (`git grep -n 'JobID:' -- internal cmd
// ':!*_test.go'` finds one line, app.go's retry path, which passes a
// history entry's own NzoID). But the ID arrives here from the API without
// re-validation — /api?mode=queue&name=delete&value=<csv> reaches RemoveJob
// unchanged — so it is untrusted at this boundary.
//
// Standing Design Rule 1's carve-out is the reason this guard stays rather
// than being argued away: a value interpolated into a path keeps its check,
// and the check says why where it sits.
func manifestPath(adminDir, jobID string) (string, error) {
	if !jobIDIsPathSafe(jobID) {
		return "", fmt.Errorf("app: manifest path: unsafe job ID %q", jobID)
	}
	return filepath.Join(manifestDir(adminDir), jobID+".json.gz"), nil
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
