package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// seedWritten records rows in written_articles for jobID through the store's
// own write path, and fails the test unless every one of them landed.
//
// ApplyRecord writes nothing for a job with no job_files row, so a fixture
// that seeds before admitting the job would otherwise seed nothing and the
// test using it would pass vacuously.
func seedWritten(t *testing.T, st *durability.Store, jobID string, rows []durability.WrittenRow) {
	t.Helper()
	if err := st.ApplyRecord(t.Context(), []durability.RecordBatch{{JobID: jobID, Rows: rows}}); err != nil {
		t.Fatalf("seedWritten: %v", err)
	}
	got, err := st.WrittenRows(t.Context(), jobID)
	if err != nil {
		t.Fatalf("seedWritten: read back: %v", err)
	}
	have := make(map[[2]int64]bool, len(got))
	for _, r := range got {
		have[[2]int64{int64(r.FileIdx), int64(r.ArtIdx)}] = true
	}
	for _, r := range rows {
		if !have[[2]int64{int64(r.FileIdx), int64(r.ArtIdx)}] {
			t.Fatalf("seedWritten: file %d article %d did not land; does the job have a job_files row?", r.FileIdx, r.ArtIdx)
		}
	}
}

// realStore is the application's *durability.Store. It fails the test if a
// double has replaced the store.
func realStore(t *testing.T, application *Application) *durability.Store {
	t.Helper()
	st, ok := application.durable.(*durability.Store)
	if !ok {
		t.Fatalf("want the application's real *durability.Store, got %T", application.durable)
	}
	return st
}
