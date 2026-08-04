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
// Nothing in the process survives to notice. The in-process half of #310 is a
// flag that is deliberately not persisted, and the guard that catches the same
// disagreement in-process
// — JobProgress.describesSameJobAs, on hydrateJobLocked and hydrateSnapshot —
// cannot fire here: SQLiteStore.Get sizes progress from the manifest it just
// read, so the pair those compare agrees with itself by construction. The
// disagreement is between the two *stored* artifacts, and only the row
// indices carry it.

// tornPair persists a job of rowFiles files, then overwrites its manifest blob
// with the same file set minus the one at dropIdx, leaving the two stored
// artifacts describing different shapes -- exactly what a crash between
// ReplaceManifest's blob write and its transaction leaves behind.
//
// The blob has to be a real subset of the rows, not an unrelated manifest:
// dropping a file renumbers every index after it while every surviving file
// keeps its subject, and that difference between the two columns is the whole
// mechanism under test. Dropping a middle file is what makes the renumber
// non-trivial.
func tornPair(t *testing.T, name string, rowFiles, dropIdx int) (*SQLiteStore, string, *Job) {
	t.Helper()
	if dropIdx < 0 || dropIdx >= rowFiles {
		t.Fatalf("fixture guard: dropIdx %d outside a %d-file job", dropIdx, rowFiles)
	}
	store, dir := setupResidencyTestStore(t)

	job := makeMultiFileJob(t, name, rowFiles, 2)
	job.Status = constants.StatusDownloading
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("fixture guard: job must be resident: %v", err)
	}
	tearManifestWrite(t, dir, job, manifestWithout(m, dropIdx))
	return store, dir, job
}

// manifestWithout rebuilds m without the file at drop, the same way
// DiscardDeferredPar2 rebuilds one around the files it keeps.
func manifestWithout(m *Manifest, drop int) *Manifest {
	var files []JobFile
	for fi := range m.NumFiles() {
		if fi == drop {
			continue
		}
		lo, hi := m.FileRange(fi)
		articles := make([]JobArticle, 0, hi-lo)
		for i := lo; i < hi; i++ {
			articles = append(articles, JobArticle{
				ID:     m.ArticleID(i),
				Bytes:  m.ArticleBytes(i),
				Number: m.ArticleNumber(i),
			})
		}
		files = append(files, JobFile{
			Subject:        m.FileSubject(fi),
			Date:           m.FileDate(fi),
			Bytes:          m.FileBytes(fi),
			Articles:       articles,
			IsPar2Recovery: m.FileIsPar2Recovery(fi),
		})
	}
	return newManifest(files)
}

// A restart must not bind pre-discard job_files rows onto a post-discard
// manifest by file_index. Dropping a file renumbers every index after it, so
// the surviving files would inherit their old neighbours' per-file state —
// the #310 splice, reached through a crash instead of a checkpoint, and
// permanent because nothing re-runs the reconciliation after a restart.
func TestGet_ReconcilesRowsThatOutnumberTheStoredManifest(t *testing.T) {
	store, _, job := tornPair(t, "restart-shape", 3, 1)

	loaded, err := store.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded == nil {
		t.Fatal("Get dropped the job entirely; a damaged job must stay in the queue")
	}

	// The job has to come back usable. Degrading it to non-resident would
	// leave it at StatusDownloading with a nil manifest, which
	// ForEachUnfinishedArticle skips and PromoteNext never considers -- a job
	// reporting "Downloading" and dispatching nothing for the life of the
	// process.
	if loaded.manifest == nil {
		t.Fatal("Get left the job non-resident, so nothing will ever dispatch its remaining articles")
	}
	if loaded.progress == nil {
		t.Fatal("Get left progress nil on a resident job")
	}
	if !loaded.progress.describesSameJobAs(loaded.manifest) {
		t.Errorf("progress (%d files) still disagrees with the manifest (%d files) after reconciliation",
			len(loaded.progress.files), loaded.manifest.NumFiles())
	}

	// The stored rows must agree now too, or the next restart repeats this.
	rows, err := store.storedFileRows(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("storedFileRows: %v", err)
	}
	if len(rows) != loaded.manifest.NumFiles() {
		t.Errorf("job_files still holds %d rows for a %d-file manifest; the rewrite did not land",
			len(rows), loaded.manifest.NumFiles())
	}
	for i, r := range rows {
		if r.idx != i {
			t.Errorf("row %d has file_index %d; indices are not contiguous from zero", i, r.idx)
		}
		if want := loaded.manifest.FileSubject(i); r.subject != want {
			t.Errorf("row %d subject %q, want %q", i, r.subject, want)
		}
	}
}

// Reconciliation must carry per-file progress across the renumber. Matching on
// the index would attribute a surviving file's downloaded bytes to whichever
// file inherited its slot; rebuilding from the manifest alone would zero them
// and re-download a job that was nearly finished. The subject is the identity
// that survives.
func TestGet_ReconciliationCarriesProgressAcrossTheRenumber(t *testing.T) {
	store, _, job := tornPair(t, "restart-carry", 3, 1)

	before, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	// File C survives the drop of B, so it must move from index 2 to 1.
	movedSubject := before.FileSubject(2)

	// Mark every article of that file done on the pre-tear job and encode it
	// the way the store does, so the row carries a real articles_done bitmap.
	// BytesDownloaded is deliberately not seeded: recompute derives it from
	// these bits, so the bitmap is the state that has to survive and the
	// bytes are the evidence that it did.
	lo, hi := before.FileRange(2)
	for i := lo; i < hi; i++ {
		job.progress.done.Set(i)
	}
	artDone := encodeArticlesDone(job, 2)
	if artDone == "" {
		t.Fatal("fixture guard: encoded an empty articles_done bitmap, so the carry is untestable")
	}
	if _, err := store.db.ExecContext(t.Context(),
		`UPDATE job_files SET articles_done = ?, filename = ?, write_cursor = ?, assembled_crc32 = ?, complete = 1
		 WHERE job_id = ? AND subject = ?`,
		artDone, "carried.bin", int64(4096), uint32(0xDEADBEEF), job.ID, movedSubject); err != nil {
		t.Fatalf("seed row state: %v", err)
	}

	loaded, err := store.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.manifest == nil {
		t.Fatal("job came back non-resident")
	}

	idx := -1
	for i := range loaded.manifest.NumFiles() {
		if loaded.manifest.FileSubject(i) == movedSubject {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("fixture guard: the reconciled manifest no longer contains %q, so nothing exercises the carry", movedSubject)
	}
	if idx == 2 {
		t.Fatal("fixture guard: the file did not move index, so this does not exercise a renumber")
	}

	if got := loaded.progress.FileFilename(idx); got != "carried.bin" {
		t.Errorf("filename %q at index %d, want %q -- state did not follow the file across the renumber", got, idx, "carried.bin")
	}
	if got := loaded.progress.FileWriteCursor(idx); got != 4096 {
		t.Errorf("write_cursor %d at index %d, want 4096", got, idx)
	}
	if got := loaded.progress.FileAssembledCRC32(idx); got != 0xDEADBEEF {
		t.Errorf("assembled_crc32 %#x at index %d, want 0xDEADBEEF", got, idx)
	}
	if !loaded.progress.FileComplete(idx) {
		t.Errorf("file %d lost its complete flag across the renumber", idx)
	}

	// The bitmap itself: every article of the moved file must read done at
	// its new index, which is what stops a nearly-finished job re-downloading.
	nlo, nhi := loaded.manifest.FileRange(idx)
	for i := nlo; i < nhi; i++ {
		if !loaded.progress.done.Get(i) {
			t.Errorf("article %d of the moved file is not done; articles_done did not carry", i)
			break
		}
	}
	if got := loaded.progress.FileBytesDownloaded(idx); got == 0 {
		t.Error("recompute derived zero downloaded bytes, so the carried bitmap did not reach it")
	}
}

// The detection itself, independent of Get's degradation policy: a row whose
// file_index falls outside the manifest is the disagreement, and it must be
// reported rather than skipped. The pre-existing bounds check dropped such
// rows silently, which is what let the remaining in-range rows splice.
func TestRestoreJobProgress_ReportsARowIndexPastTheManifest(t *testing.T) {
	store, dir, job := tornPair(t, "restore-shape", 3, 1)

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
