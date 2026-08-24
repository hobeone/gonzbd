package queue

import (
	"testing"
)

// TestInsertJobFilesTx_WritesEveryPerFileColumn pins that the INSERT carries
// each per-file value from JobProgress rather than a literal zero.
//
// This is the failure mode #287 was: `deferred` was hard-coded to 0 in the
// only writer of the day, so on-demand par2 deferred each recovery volume in
// memory, the INSERT wrote it back as not-deferred, and the first promotion
// read that over the live flag — a feature whose whole purpose is to skip
// those volumes downloaded them. A column this function forgets is a column
// that silently reverts.
//
// The values below are deliberately all distinct and non-zero, so a row
// assembled with two arguments transposed fails rather than coincidentally
// matching: the INSERT passes twelve positional arguments, and neither the
// compiler nor the schema can catch a swap between two INTEGER columns.
func TestInsertJobFilesTx_WritesEveryPerFileColumn(t *testing.T) {
	store, _, db := setupResidencyTestStoreWithDB(t)
	job := makeMultiFileJob(t, "insert-job-files", 2, 4)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	p := job.Progress()

	// Give file 0 state that is distinguishable in every column the INSERT
	// writes from progress, and leave file 1 as the untouched contrast.
	p.markDone(m, 0)   // two articles resolved done
	p.markDone(m, 1)   //
	p.markFailed(m, 2) // one resolved failed (and therefore also done)
	p.files[0].AssembledCRC32 = 0xDEADBEEF
	p.files[0].Filename = "resolved-name.rar"
	p.files[0].Complete = true
	p.files[1].Fetch = FetchIfNeeded

	// Add creates the jobs row (job_files.job_id references it) and a first
	// set of job_files rows. Clear those and call insertJobFilesTx directly:
	// it documents that it assumes no rows exist for the job yet, since
	// file_index is the key, so this is both the direct call the test needs
	// and the precondition addTx (its only caller now that ReplaceManifest
	// is gone) already honours.
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM job_files WHERE job_id = ?`, job.ID); err != nil {
		t.Fatalf("clear job_files: %v", err)
	}

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := insertJobFilesTx(t.Context(), tx, job, m); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insertJobFilesTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	type row struct {
		complete, fetch int
		bytes           int64
		filename        string
		crc32           uint32
		articleCount    int
	}
	read := func(idx int) row {
		t.Helper()
		var r row
		err := db.QueryRowContext(t.Context(), `
SELECT complete, fetch_policy, bytes,
       COALESCE(filename, ''), assembled_crc32, article_count
FROM job_files WHERE job_id = ? AND file_index = ?`, job.ID, idx).
			Scan(&r.complete, &r.fetch, &r.bytes,
				&r.filename, &r.crc32, &r.articleCount)
		if err != nil {
			t.Fatalf("read row %d: %v", idx, err)
		}
		return r
	}

	got0 := read(0)
	if got0.complete != 1 {
		t.Errorf("file 0 complete = %d, want 1", got0.complete)
	}
	if got0.fetch != int(FetchAlways) {
		t.Errorf("file 0 fetch_policy = %d, want %d", got0.fetch, FetchAlways)
	}
	if got0.filename != "resolved-name.rar" {
		t.Errorf("file 0 filename = %q, want %q", got0.filename, "resolved-name.rar")
	}
	if got0.crc32 != 0xDEADBEEF {
		t.Errorf("file 0 assembled_crc32 = %#x, want %#x", got0.crc32, 0xDEADBEEF)
	}
	if want := m.FileBytes(0); got0.bytes != want {
		t.Errorf("file 0 bytes = %d, want %d", got0.bytes, want)
	}
	if got0.articleCount != 4 {
		t.Errorf("file 0 article_count = %d, want 4", got0.articleCount)
	}

	// File 1 is the contrast: deferred, not complete, no resolved name.
	got1 := read(1)
	if got1.fetch != int(FetchIfNeeded) {
		t.Errorf("file 1 fetch_policy = %d, want %d (a deferred volume written as 0 is #287)", got1.fetch, FetchIfNeeded)
	}
	if got1.complete != 0 {
		t.Errorf("file 1 complete = %d, want 0", got1.complete)
	}
	if got1.filename != "" {
		t.Errorf("file 1 filename = %q, want empty", got1.filename)
	}
}
