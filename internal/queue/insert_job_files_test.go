package queue

import (
	"testing"
)

// insertJobFilesTx is shared by addTx and ReplaceManifest, the only two
// writers of a job_files row's full shape. Its contract is that every
// per-file value comes from the job's progress rather than from a literal:
// #287 was a hard-coded `deferred = 0` in the sole writer of the day, which
// meant on-demand par2 downloaded exactly the volumes it exists to skip, in
// every configuration with a store.
//
// These tests cover it directly. It had no direct test before, having been
// extracted in #308 and exercised only through its two callers.

// Per-file mutable state must round-trip out of JobProgress, and the
// manifest-derived columns must line up with the file they describe.
func TestInsertJobFilesTx_TakesPerFileStateFromProgress(t *testing.T) {
	store, _, db := setupResidencyTestStoreWithDB(t)

	job := makeMultiFileJobWithPar2(t, "insert-rows", 2, 3)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("fixture guard: job must be resident: %v", err)
	}
	// Set the two flags that #287 hard-coded, on different files, so a
	// literal in either column shows up as a mismatch rather than agreeing
	// with the default by accident.
	job.progress.files[0].Complete = true
	job.progress.files[1].Deferred = true
	job.progress.files[1].BytesDownloaded = 4096
	job.progress.files[0].Filename = "file-a.bin"

	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Add already wrote a row set through this same helper; clear it so the
	// call below is the one under test, writing against the flags set above.
	if _, err := db.ExecContext(t.Context(), `DELETE FROM job_files WHERE job_id = ?`, job.ID); err != nil {
		t.Fatalf("clear job_files: %v", err)
	}

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := insertJobFilesTx(t.Context(), tx, job, m); err != nil {
		t.Fatalf("insertJobFilesTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rows, err := db.QueryContext(t.Context(),
		`SELECT file_index, subject, bytes, is_par2_recovery, complete, deferred, bytes_downloaded, filename, article_count
		 FROM job_files WHERE job_id = ? ORDER BY file_index ASC`, job.ID)
	if err != nil {
		t.Fatalf("query job_files: %v", err)
	}
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var idx, isPar2, complete, deferred, articleCount int
		var subject, filename string
		var bytes, bytesDownloaded int64
		if err := rows.Scan(&idx, &subject, &bytes, &isPar2, &complete, &deferred, &bytesDownloaded, &filename, &articleCount); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++

		if want := m.FileSubject(idx); subject != want {
			t.Errorf("file %d: subject %q, want %q", idx, subject, want)
		}
		if want := m.FileBytes(idx); bytes != want {
			t.Errorf("file %d: bytes %d, want %d", idx, bytes, want)
		}
		wantPar2 := 0
		if m.FileIsPar2Recovery(idx) {
			wantPar2 = 1
		}
		if isPar2 != wantPar2 {
			t.Errorf("file %d: is_par2_recovery %d, want %d", idx, isPar2, wantPar2)
		}
		// article_count is what lets a restart size JobProgress without
		// decompressing every manifest, so it must be the file's real span.
		lo, hi := m.FileRange(idx)
		if articleCount != hi-lo {
			t.Errorf("file %d: article_count %d, want %d", idx, articleCount, hi-lo)
		}

		wantComplete := 0
		if job.progress.FileComplete(idx) {
			wantComplete = 1
		}
		if complete != wantComplete {
			t.Errorf("file %d: complete %d, want %d — the column is not being read from progress", idx, complete, wantComplete)
		}
		wantDeferred := 0
		if job.progress.FileDeferred(idx) {
			wantDeferred = 1
		}
		if deferred != wantDeferred {
			t.Errorf("file %d: deferred %d, want %d — this is the #287 shape", idx, deferred, wantDeferred)
		}
		if want := job.progress.FileBytesDownloaded(idx); bytesDownloaded != want {
			t.Errorf("file %d: bytes_downloaded %d, want %d", idx, bytesDownloaded, want)
		}
		if want := job.progress.FileFilename(idx); filename != want {
			t.Errorf("file %d: filename %q, want %q", idx, filename, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != m.NumFiles() {
		t.Errorf("wrote %d rows for a %d-file manifest", seen, m.NumFiles())
	}
}

// The doc comment's "callers replacing an existing set must delete it first"
// is enforced by the schema, not by the function. Pinning it here means a
// caller that drops its DELETE fails loudly rather than half-writing a set.
func TestInsertJobFilesTx_RefusesASecondInsertForTheSameJob(t *testing.T) {
	store, _, db := setupResidencyTestStoreWithDB(t)

	job := makeMultiFileJob(t, "insert-twice", 2, 1)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("fixture guard: job must be resident: %v", err)
	}
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	err = insertJobFilesTx(t.Context(), tx, job, m)
	_ = tx.Rollback()
	if err == nil {
		t.Fatal("a second insert for the same job succeeded; the row set would be doubled rather than replaced")
	}
}
