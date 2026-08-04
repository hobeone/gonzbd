package queue

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// The crash window Store.ReplaceManifest cannot close: it writes the rebuilt
// manifest blob and then, in a separate transaction, rewrites job_files. A
// crash between the two leaves the new, smaller blob paired with the
// pre-discard rows.
//
// Nothing in the process survives to notice. manifestRowsStale (#310) is
// in-memory only, and the guard that catches the same disagreement in-process
// — JobProgress.describesSameJobAs, on hydrateJobLocked and hydrateSnapshot —
// cannot fire here: SQLiteStore.Get sizes progress from the manifest it just
// read, so the pair those compare agrees with itself by construction. The
// disagreement is between the two *stored* artifacts, and only the row
// indices carry it.

// tornPair persists a job of rowFiles files, then overwrites its manifest blob
// with one describing manifestFiles, leaving the two stored artifacts
// describing different file sets. It returns the store, its directory and the
// persisted job.
func tornPair(t *testing.T, name string, rowFiles, manifestFiles int) (*SQLiteStore, string, *Job) {
	t.Helper()
	store, dir := setupResidencyTestStore(t)

	job := makeMultiFileJob(t, name, rowFiles, 2)
	job.Status = constants.StatusDownloading
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	smaller := makeMultiFileJob(t, name+"-smaller", manifestFiles, 2)
	m, err := smaller.Manifest()
	if err != nil {
		t.Fatalf("fixture guard: the smaller job must be resident: %v", err)
	}
	if m.NumFiles() >= rowFiles {
		t.Fatalf("fixture guard: manifest of %d files does not shrink %d rows", m.NumFiles(), rowFiles)
	}
	tearManifestWrite(t, dir, job, m)
	return store, dir, job
}

// A restart must not bind pre-discard job_files rows onto a post-discard
// manifest by file_index. Dropping a file renumbers every index after it, so
// the surviving files would inherit their old neighbours' per-file state —
// the #310 splice, reached through a crash instead of a checkpoint, and
// permanent because nothing re-runs the reconciliation after a restart.
func TestGet_RefusesRowsThatOutnumberTheStoredManifest(t *testing.T) {
	store, _, job := tornPair(t, "restart-shape", 3, 2)

	loaded, err := store.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Get returned an error rather than a degraded job: %v", err)
	}
	if loaded == nil {
		t.Fatal("Get dropped the job entirely; a damaged job must stay in the queue")
	}
	// Refusing the pair means refusing residency. Queue.Load then rebuilds
	// progress from job_files, which is the shape both stored artifacts
	// agreed on before the discard — the state a restart was always
	// documented to land in.
	if loaded.manifest != nil {
		t.Error("Get left the job resident on a manifest its stored rows contradict, so the spliced progress is live")
	}
	if loaded.progress != nil {
		t.Error("Get kept progress built from the contradicted manifest; Queue.Load must rebuild it from job_files instead")
	}
}

// The detection itself, independent of Get's degradation policy: a row whose
// file_index falls outside the manifest is the disagreement, and it must be
// reported rather than skipped. The pre-existing bounds check dropped such
// rows silently, which is what let the remaining in-range rows splice.
func TestRestoreJobProgress_ReportsARowIndexPastTheManifest(t *testing.T) {
	store, dir, job := tornPair(t, "restore-shape", 3, 2)

	// Rebuild the job the way Get does, from the shrunk blob, so progress is
	// sized to two files while the stored rows still hold three.
	var shrunk Manifest
	if err := readGzJSON(filepath.Join(dir, "manifests", job.ID+".json.gz"), &shrunk); err != nil {
		t.Fatalf("read shrunk manifest: %v", err)
	}
	rebuilt := &Job{ID: job.ID, Status: job.Status}
	rebuilt.manifest = &shrunk
	rebuilt.progress = newJobProgress(&shrunk)

	err := store.RestoreJobProgress(t.Context(), rebuilt)
	if err == nil {
		t.Fatal("RestoreJobProgress accepted rows describing more files than the manifest")
	}
	if !errors.Is(err, ErrManifestStale) {
		t.Errorf("error %v is not ErrManifestStale, so callers cannot distinguish it from an I/O failure", err)
	}
}
