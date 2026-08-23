package durability

import (
	"context"
	"testing"
)

// TestCoveredByAny pins the art_idx coverage predicate used to subtract
// redelivered articles: it must treat each stored run's span as inclusive on
// both ends and report false for a gap between two stored runs.
func TestCoveredByAny(t *testing.T) {
	stored := []Run{
		{FirstArtIdx: 0, LastArtIdx: 4},
		{FirstArtIdx: 10, LastArtIdx: 12},
	}
	cases := []struct {
		artIdx int32
		want   bool
	}{
		{0, true}, {4, true}, {5, false}, {9, false}, {10, true}, {12, true}, {13, false},
	}
	for _, c := range cases {
		if got := coveredByAny(stored, c.artIdx); got != c.want {
			t.Errorf("coveredByAny(_, %d) = %v, want %v", c.artIdx, got, c.want)
		}
	}
}

// TestMergeAdjacentRuns pins the fold itself, independent of SQL storage:
// empty input, a single run, and a three-way chain that must fold into one.
func TestMergeAdjacentRuns(t *testing.T) {
	if got := mergeAdjacentRuns(nil); got != nil {
		t.Errorf("mergeAdjacentRuns(nil) = %+v, want nil", got)
	}

	single := []Run{{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Offset: 0, Length: 10, CRC32: 0x1234}}
	got := mergeAdjacentRuns(single)
	if len(got) != 1 || got[0] != single[0] {
		t.Errorf("mergeAdjacentRuns(single) = %+v, want unchanged %+v", got, single)
	}

	chain := []Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Offset: 0, Length: 4, CRC32: crcOf([]byte("AAAA"))},
		{FileIdx: 0, FirstArtIdx: 1, LastArtIdx: 1, Offset: 4, Length: 4, CRC32: crcOf([]byte("BBBB"))},
		{FileIdx: 0, FirstArtIdx: 2, LastArtIdx: 2, Offset: 8, Length: 4, CRC32: crcOf([]byte("CCCC"))},
	}
	got = mergeAdjacentRuns(chain)
	if len(got) != 1 {
		t.Fatalf("mergeAdjacentRuns(chain) = %d rows, want 1: %+v", len(got), got)
	}
	if got[0].FirstArtIdx != 0 || got[0].LastArtIdx != 2 || got[0].Offset != 0 || got[0].Length != 12 {
		t.Errorf("folded run = %+v, want FirstArtIdx=0 LastArtIdx=2 Offset=0 Length=12", got[0])
	}
	want := crcOf([]byte("AAAABBBBCCCC"))
	if got[0].CRC32 != want {
		t.Errorf("folded CRC32 = %#x, want %#x", got[0].CRC32, want)
	}
}

// TestSQLiteRunStore_HelpersDirectly exercises queryBracketing, insertRuns,
// deleteRows, and commitFile against a live transaction, the way Commit
// itself calls them. Commit's own tests exercise the same code paths
// end-to-end; this pins each helper's own contract in isolation — in
// particular that deleteRows only removes the exact (job_id, file_idx,
// offset) rows it is given, and that queryBracketing's maxEnd bound excludes
// a row starting past it.
func TestSQLiteRunStore_HelpersDirectly(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	rs := NewSQLiteRunStore(db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	near := Run{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Offset: 0, Length: 10, CRC32: 0xAAAA}
	far := Run{FileIdx: 0, FirstArtIdx: 5, LastArtIdx: 5, Offset: 1000, Length: 10, CRC32: 0xBBBB}
	if err := rs.insertRuns(ctx, tx, "job-1", []Run{near, far}); err != nil {
		t.Fatal(err)
	}

	got, err := rs.queryBracketing(ctx, tx, "job-1", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != near {
		t.Fatalf("queryBracketing(maxEnd=50) = %+v, want only %+v — far starts past the bound", got, near)
	}

	if err := rs.deleteRows(ctx, tx, "job-1", 0, []Run{near}); err != nil {
		t.Fatal(err)
	}
	remaining, err := rs.queryBracketing(ctx, tx, "job-1", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0] != far {
		t.Fatalf("after deleteRows(near) = %+v, want only %+v to remain", remaining, far)
	}

	arts := []DurableArticle{
		{FileIdx: 1, ArtIdx: 0, Offset: 0, Length: 5, CRC32: crcOf([]byte("hello"))},
	}
	if err := rs.commitFile(ctx, tx, "job-1", 1, arts); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	file1, err := rs.ForFile(ctx, "job-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(file1) != 1 || file1[0].CRC32 != crcOf([]byte("hello")) {
		t.Fatalf("ForFile(1) after commitFile = %+v, want one row with hello's CRC", file1)
	}
}

// TestSQLiteRunStore_ForJobReturnsAllFilesOrdered pins ForJob's whole-job
// read: every file's runs, ordered by FileIdx then Offset.
func TestSQLiteRunStore_ForJobReturnsAllFilesOrdered(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	if err := rs.Commit(ctx, "job-1", []DurableArticle{
		{FileIdx: 1, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 0x1},
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 0x2},
		{FileIdx: 0, ArtIdx: 5, Offset: 200, Length: 10, CRC32: 0x3},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := rs.ForJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ForJob returned %d rows, want 3: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.FileIdx < prev.FileIdx || (cur.FileIdx == prev.FileIdx && cur.Offset < prev.Offset) {
			t.Fatalf("ForJob not ordered by FileIdx then Offset: %+v", got)
		}
	}
}

// TestSQLiteRunStore_ForJobIsScopedToJob pins that ForJob never returns
// another job's rows.
func TestSQLiteRunStore_ForJobIsScopedToJob(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	art := DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 0x1}
	if err := rs.Commit(ctx, "job-1", []DurableArticle{art}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Commit(ctx, "job-2", []DurableArticle{art}); err != nil {
		t.Fatal(err)
	}

	got, err := rs.ForJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForJob(job-1) returned %d rows, want 1 — leaked job-2's row", len(got))
	}
}

// TestSQLiteRunStore_DeleteJobIsScoped pins that DeleteJob removes exactly
// one job's rows and leaves every other job's rows — including a second
// file within the deleted job — untouched.
func TestSQLiteRunStore_DeleteJobIsScoped(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	art := DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 0x1}
	if err := rs.Commit(ctx, "job-1", []DurableArticle{art}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Commit(ctx, "job-2", []DurableArticle{art}); err != nil {
		t.Fatal(err)
	}

	if err := rs.DeleteJob(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}

	if got, _ := rs.ForJob(ctx, "job-1"); len(got) != 0 {
		t.Errorf("job-1 has %d runs after DeleteJob, want 0", len(got))
	}
	if got, _ := rs.ForJob(ctx, "job-2"); len(got) != 1 {
		t.Errorf("DeleteJob(job-1) removed job-2's runs")
	}
}

// TestSQLiteRunStore_CommitEmptyIsNoop pins that an empty batch neither
// errors nor opens a transaction against a store that has nothing else to
// give it a schema — a closed DB would otherwise surface a BeginTx error
// even though there is nothing to commit.
func TestSQLiteRunStore_CommitEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rs := NewSQLiteRunStore(db)
	if err := rs.Commit(ctx, "job-1", nil); err != nil {
		t.Fatalf("Commit(nil) on a closed DB = %v, want nil (nothing to do)", err)
	}
}

// TestSQLiteRunStore_CommitErrorsOnClosedDB covers the BeginTx error path.
func TestSQLiteRunStore_CommitErrorsOnClosedDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rs := NewSQLiteRunStore(db)
	err := rs.Commit(ctx, "job-1", []DurableArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 1}})
	if err == nil {
		t.Fatal("Commit on a closed DB returned nil, want an error")
	}
}

// TestSQLiteRunStore_DeleteJobErrorsOnClosedDB covers DeleteJob's error path.
func TestSQLiteRunStore_DeleteJobErrorsOnClosedDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rs := NewSQLiteRunStore(db)
	if err := rs.DeleteJob(ctx, "job-1"); err == nil {
		t.Fatal("DeleteJob on a closed DB returned nil, want an error")
	}
}

// TestSQLiteRunStore_DeleteFileIsScopedAndReportsAFailure covers the file-
// scoped deletion Resumer's gate performs.
//
// Both halves are the point. The SCOPE is what stops one disproved partial
// from costing a job its other files' records; the ERROR is what stops the
// sweep reporting a discard that did not reach the store, which would leave
// the next start adopting runs the file has already contradicted.
func TestSQLiteRunStore_DeleteFileIsScopedAndReportsAFailure(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	rs := NewSQLiteRunStore(db)
	if err := rs.Commit(ctx, "job-1", []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
		{FileIdx: 1, ArtIdx: 5, Offset: 0, Length: 100, CRC32: 2},
	}); err != nil {
		t.Fatal(err)
	}
	// Another job's file 0, so the delete's job scoping is exercised too.
	if err := rs.Commit(ctx, "job-2", []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 3},
	}); err != nil {
		t.Fatal(err)
	}

	if err := rs.DeleteFile(ctx, "job-1", 0); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if got, err := rs.ForFile(ctx, "job-1", 0); err != nil || len(got) != 0 {
		t.Errorf("job-1 file 0 holds %d runs (err %v), want none", len(got), err)
	}
	if got, err := rs.ForFile(ctx, "job-1", 1); err != nil || len(got) != 1 {
		t.Errorf("job-1 file 1 holds %d runs (err %v), want 1 — a resume stat'ed one "+
			"path and proved nothing about the others", len(got), err)
	}
	if got, err := rs.ForFile(ctx, "job-2", 0); err != nil || len(got) != 1 {
		t.Errorf("job-2 file 0 holds %d runs (err %v), want 1", len(got), err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rs.DeleteFile(ctx, "job-1", 1); err == nil {
		t.Fatal("DeleteFile on a closed DB returned nil; the sweep would report a discard " +
			"that never reached the store, and the next start adopts the runs the file " +
			"has already contradicted")
	}
}

// TestSQLiteRunStore_InsertFailureMidBatchRollsBack pins insertRuns' error
// path and the transaction's atomicity together: a trigger fails the second
// insert, and the first file's already-computed merge must not survive
// partially — Commit is per-call atomic across every file it touches.
func TestSQLiteRunStore_InsertFailureMidBatchRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	rs := NewSQLiteRunStore(db)

	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER abort_file_1 BEFORE INSERT ON durable_runs
		WHEN NEW.file_idx = 1 BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}

	err := rs.Commit(ctx, "job-1", []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 1},
		{FileIdx: 1, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 2},
	})
	if err == nil {
		t.Fatal("Commit succeeded despite the trigger")
	}

	got, loadErr := rs.ForJob(ctx, "job-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(got) != 0 {
		t.Fatalf("ForJob returned %d rows after a rolled-back commit, want 0: %+v", len(got), got)
	}
}

// TestSQLiteRunStore_DeleteFailureMidBatchRollsBack pins deleteRows' error
// path: a trigger fails the delete of a stored row a merge consumed, and the
// whole commit — including the insert of the merged replacement — must not
// land.
func TestSQLiteRunStore_DeleteFailureMidBatchRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	rs := NewSQLiteRunStore(db)

	first := []byte("AAAA")
	if err := rs.Commit(ctx, "job-1", []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: int32(len(first)), CRC32: crcOf(first)},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER abort_delete BEFORE DELETE ON durable_runs
		WHEN OLD.offset = 0 BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}

	second := []byte("BBBB")
	err := rs.Commit(ctx, "job-1", []DurableArticle{
		{FileIdx: 0, ArtIdx: 1, Offset: int64(len(first)), Length: int32(len(second)), CRC32: crcOf(second)},
	})
	if err == nil {
		t.Fatal("Commit succeeded despite the delete trigger")
	}

	got, loadErr := rs.ForFile(ctx, "job-1", 0)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(got) != 1 || got[0].CRC32 != crcOf(first) {
		t.Fatalf("stored row changed after a rolled-back commit: %+v, want the original 'AAAA' row untouched", got)
	}
}
