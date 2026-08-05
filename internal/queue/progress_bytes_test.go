package queue

import "testing"

func TestNewJobProgress_CarriesPerFileBytes(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "payload.rar", Bytes: 5000, Articles: []JobArticle{{ID: "a1", Bytes: 2500}, {ID: "a2", Bytes: 2500}}},
		{Subject: "payload.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "b1", Bytes: 800}}},
	})
	p := newJobProgress(m)

	if got, want := p.files[0].Bytes, int64(5000); got != want {
		t.Errorf("files[0].Bytes = %d, want %d", got, want)
	}
	if got, want := p.files[1].Bytes, int64(800); got != want {
		t.Errorf("files[1].Bytes = %d, want %d", got, want)
	}
}

// TestNewJobProgressSized_RoundTripsPerFileState builds a job, downloads
// part of it (one file fully complete via MarkFileComplete, one file
// partially downloaded, one file deferred), persists it, then reconstructs
// progress the non-resident way via ArticleCountsByJob and
// newJobProgressSized. It asserts the reconstruction's per-file
// BytesDownloaded/Complete/Deferred match what MarkArticlesDone/
// MarkFileComplete/the direct Deferred write actually stored, and that
// TotalRemainingBytes still agrees with the residency-parity guarantee
// TestTotalRemainingBytes_RestartReconstructsNonResident pins.
func TestNewJobProgressSized_RoundTripsPerFileState(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "sized-roundtrip", 3, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// File 0: fully downloaded and marked complete.
	if err := q.MarkArticlesDone(job.ID, []string{articleID(0, 0), articleID(0, 1)}); err != nil {
		t.Fatalf("MarkArticlesDone file 0: %v", err)
	}
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete file 0: %v", err)
	}

	// File 1: partially downloaded, not complete.
	if err := q.MarkArticlesDone(job.ID, []string{articleID(1, 0)}); err != nil {
		t.Fatalf("MarkArticlesDone file 1: %v", err)
	}

	// File 2: deferred, untouched otherwise.
	job.progress.files[2].Deferred = true

	if err := store.Update(t.Context(), job); err != nil {
		t.Fatalf("Update: %v", err)
	}

	metas, err := store.ArticleCountsByJob(t.Context())
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	sized := newJobProgressSized(metas[job.ID])

	if got, want := sized.files[0].Complete, true; got != want {
		t.Errorf("file 0 Complete = %v, want %v", got, want)
	}
	if got, want := sized.files[0].BytesDownloaded, job.progress.files[0].BytesDownloaded; got != want {
		t.Errorf("file 0 BytesDownloaded = %d, want %d", got, want)
	}
	if got, want := sized.files[1].Complete, false; got != want {
		t.Errorf("file 1 Complete = %v, want %v", got, want)
	}
	if got, want := sized.files[1].BytesDownloaded, job.progress.files[1].BytesDownloaded; got != want {
		t.Errorf("file 1 BytesDownloaded = %d, want %d", got, want)
	}
	if got, want := sized.files[2].Deferred, true; got != want {
		t.Errorf("file 2 Deferred = %v, want %v", got, want)
	}
	if got, want := sized.files[2].BytesDownloaded, int64(0); got != want {
		t.Errorf("file 2 BytesDownloaded = %d, want %d", got, want)
	}

	// sized.RemainingBytes() reads the still-maintained field, seeded in
	// newJobProgressSized as sum(Bytes-BytesDownloaded) over files that are
	// neither Complete nor Deferred — file 0 (complete) and file 2
	// (deferred) contribute nothing, file 1 contributes its undownloaded
	// half. This is deliberately not compared against the resident job's
	// own RemainingBytes(): the maintained counter on job.progress does not
	// yet exclude deferred files (that exclusion is what Task 2/3 of the
	// plan add), so the two are expected to disagree whenever a file is
	// deferred. The residency-parity property for the no-deferral case is
	// pinned separately by TestRemainingBytes_IdenticalResidentAndNonResident
	// once the derivation lands (plan Task 4).
	if got, want := sized.RemainingBytes(), int64(100_000); got != want {
		t.Errorf("sized.RemainingBytes() = %d, want %d (file1's undownloaded half only)", got, want)
	}
}

func TestRestoreJobProgress_CarriesPerFileBytes(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "restore-bytes", 3, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	restored := &Job{ID: job.ID, manifest: m, progress: newJobProgress(m)}
	for fi := range restored.progress.files {
		restored.progress.files[fi].Bytes = 0 // prove the restore sets it, not the constructor
	}

	if err := store.RestoreJobProgress(t.Context(), restored); err != nil {
		t.Fatalf("RestoreJobProgress: %v", err)
	}
	for fi := range m.NumFiles() {
		if got, want := restored.progress.files[fi].Bytes, m.FileBytes(fi); got != want {
			t.Errorf("restored files[%d].Bytes = %d, want %d", fi, got, want)
		}
	}
}
