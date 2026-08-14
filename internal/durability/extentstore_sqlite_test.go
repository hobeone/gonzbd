package durability

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSQLiteExtentStore_CommitRoundTrip(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))

	bm := NewBitmap(100)
	bm.Set(0)
	bm.Set(42)
	bm.Set(99)
	ext := FileExtent{
		FileIdx: 3, Durable: bm, VerifiedTo: 4096, PrefixCRC: 0xABCD, HasPrefixCRC: true,
		BytesDurable: 3000, Size: 8192, ModTimeNs: 1_700_000_000_000_000_000,
	}
	if err := es.Commit(ctx, "job-1", []FileExtent{ext}); err != nil {
		t.Fatal(err)
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("Load returned %d extents, want 1", len(got))
	}
	g := got[0]
	if g.FileIdx != 3 || g.VerifiedTo != 4096 || g.PrefixCRC != 0xABCD || !g.HasPrefixCRC {
		t.Errorf("scalar round trip wrong: %+v", g)
	}
	if g.BytesDurable != 3000 || g.Size != 8192 || g.ModTimeNs != 1_700_000_000_000_000_000 {
		t.Errorf("cache/stamp round trip wrong: %+v", g)
	}
	if g.Durable.Count() != 3 || !g.Durable.Get(0) || !g.Durable.Get(42) || !g.Durable.Get(99) {
		t.Errorf("bitmap round trip wrong: count=%d", g.Durable.Count())
	}
}

// TestSQLiteExtentStore_HasPrefixCRCRoundTrips pins that a prefix CRC which is
// genuinely zero stays distinguishable from no prefix CRC at all.
//
// There is no Class A counterpart to this test any more. ArticleFact used to
// carry a HasCRC companion for the same reason, and it was removed once every
// decode path was made to produce a checksum: a per-article CRC is never
// absent, so there is nothing to distinguish. A whole-file CRC still can be
// absent, which is why this flag and this test remain.
//
// Both extents below carry PrefixCRC == 0 and differ only in HasPrefixCRC, so
// the test fails if either side of that boolean is hardcoded. Without it every
// fixture in this file sets HasPrefixCRC: true and the flag is unprotected —
// exactly the gap the Task 4 review found by mutation.
//
// R23 makes this load-bearing: QuickCheck reads the flag, not the value, to
// decide whether a file's CRC can be compared against par2's. A read side
// degraded to true reports a fabricated CRC of zero as authoritative.
func TestSQLiteExtentStore_HasPrefixCRCRoundTrips(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))

	if err := es.Commit(ctx, "job-1", []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), PrefixCRC: 0, HasPrefixCRC: false},
		{FileIdx: 1, Durable: NewBitmap(8), PrefixCRC: 0, HasPrefixCRC: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Load returned %d extents, want 2", len(got))
	}
	if got[0].HasPrefixCRC {
		t.Error("HasPrefixCRC = true for the extent that has no prefix CRC, want false")
	}
	if !got[1].HasPrefixCRC {
		t.Error("HasPrefixCRC = false for the extent whose CRC is genuinely zero, want true")
	}
	if got[0].PrefixCRC != 0 || got[1].PrefixCRC != 0 {
		t.Fatalf("PrefixCRC = %d/%d, want 0/0 — the flag, not the value, carries the distinction",
			got[0].PrefixCRC, got[1].PrefixCRC)
	}
}

// TestSQLiteExtentStore_CommitIsAtomic pins that a job's files are never
// observable half-committed. A barrier that fails partway must leave the
// previous committed cache intact (R7).
//
// This test pins the VALIDATION loop, not the transaction. The bad row is
// rejected before any write, so removing the transaction changes nothing it
// can see — file_extents has no CHECK constraint, so nothing else can fail
// mid-batch either. TestSQLiteExtentStore_TransactionRollsBackMidBatch below
// is what pins the transaction; the two mechanisms need two tests, not two
// mutations of one test.
func TestSQLiteExtentStore_CommitIsAtomic(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	es := NewSQLiteExtentStore(db)

	first := []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: 100},
		{FileIdx: 1, Durable: NewBitmap(8), VerifiedTo: 200},
	}
	if err := es.Commit(ctx, "job-1", first); err != nil {
		t.Fatal(err)
	}

	// A second commit whose second element is invalid must roll the whole
	// batch back, leaving VerifiedTo at 100/200 rather than 999/200.
	bad := []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: 999},
		{FileIdx: 1, Durable: Bitmap{}, VerifiedTo: -1}, // negative extent is rejected
	}
	if err := es.Commit(ctx, "job-1", bad); !errors.Is(err, ErrInvalidExtent) {
		t.Fatalf("Commit error = %v, want ErrInvalidExtent", err)
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Load returned %d extents, want 2", len(got))
	}
	if got[0].VerifiedTo != 100 {
		t.Fatalf("VerifiedTo = %d after a failed commit, want 100 — the batch was not atomic", got[0].VerifiedTo)
	}
}

// TestSQLiteExtentStore_TransactionRollsBackMidBatch pins the transaction
// itself, which CommitIsAtomic cannot reach: validation rejects malformed
// input before any write, so only a failure arriving DURING the write
// exercises the rollback. In production that is ENOSPC, SQLITE_BUSY, or a
// cancelled context; here a test-only trigger produces the same shape
// deterministically, with no driver wrapper and no schema change — the
// trigger lives in this test's scratch database only.
//
// Without the transaction, row 0's new value is already committed when row 1
// fails, which violates R7 on a path validation can never guard.
func TestSQLiteExtentStore_TransactionRollsBackMidBatch(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	es := NewSQLiteExtentStore(db)

	if err := es.Commit(ctx, "job-1", []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: 100},
		{FileIdx: 1, Durable: NewBitmap(8), VerifiedTo: 200},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER abort_second BEFORE INSERT ON file_extents
		WHEN NEW.file_idx = 1 BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}

	// Both rows are valid, so the validation loop passes them; the second
	// fails at the storage layer.
	err := es.Commit(ctx, "job-1", []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: 999},
		{FileIdx: 1, Durable: NewBitmap(8), VerifiedTo: 888},
	})
	if err == nil {
		t.Fatal("Commit succeeded despite the trigger")
	}

	got, loadErr := es.Load(ctx, "job-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(got) != 2 {
		t.Fatalf("Load returned %d extents, want 2", len(got))
	}
	if got[0].VerifiedTo != 100 {
		t.Fatalf("file 0 VerifiedTo = %d, want 100 — a mid-batch failure left a partial write",
			got[0].VerifiedTo)
	}
	if got[1].VerifiedTo != 200 {
		t.Fatalf("file 1 VerifiedTo = %d, want 200", got[1].VerifiedTo)
	}
}

// TestSQLiteExtentStore_CommitRejectsEachNegativeField pins all three clauses
// of Commit's validation loop independently. Only VerifiedTo was previously
// exercised — deleting the Size or BytesDurable guard was invisible to the
// suite. BytesDurable is the R26 "bytes durable" aggregate.
func TestSQLiteExtentStore_CommitRejectsEachNegativeField(t *testing.T) {
	tests := []struct {
		name string
		ext  FileExtent
	}{
		{"VerifiedTo", FileExtent{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: -1}},
		{"Size", FileExtent{FileIdx: 0, Durable: NewBitmap(8), Size: -1}},
		{"BytesDurable", FileExtent{FileIdx: 0, Durable: NewBitmap(8), BytesDurable: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			es := NewSQLiteExtentStore(openTestDB(t))
			err := es.Commit(ctx, "job-1", []FileExtent{tc.ext})
			if !errors.Is(err, ErrInvalidExtent) {
				t.Fatalf("Commit error = %v, want ErrInvalidExtent for a negative %s", err, tc.name)
			}
		})
	}
}

func TestSQLiteExtentStore_CommitOverwritesPriorExtent(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	bm1 := NewBitmap(8)
	bm1.Set(0)
	if err := es.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: bm1, VerifiedTo: 100}}); err != nil {
		t.Fatal(err)
	}
	bm2 := NewBitmap(8)
	bm2.Set(0)
	bm2.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: bm2, VerifiedTo: 200}}); err != nil {
		t.Fatal(err)
	}
	got, _ := es.Load(ctx, "job-1")
	if len(got) != 1 {
		t.Fatalf("Load returned %d rows, want 1 — Commit inserted instead of replacing", len(got))
	}
	if got[0].VerifiedTo != 200 || got[0].Durable.Count() != 2 {
		t.Errorf("second commit did not replace the first: %+v", got[0])
	}
}

func TestSQLiteExtentStore_DeleteJobIsScoped(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	ext := FileExtent{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: 10}
	if err := es.Commit(ctx, "job-1", []FileExtent{ext}); err != nil {
		t.Fatal(err)
	}
	if err := es.Commit(ctx, "job-2", []FileExtent{ext}); err != nil {
		t.Fatal(err)
	}
	if err := es.DeleteJob(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := es.Load(ctx, "job-1"); len(got) != 0 {
		t.Errorf("job-1 has %d extents after DeleteJob, want 0", len(got))
	}
	if got, _ := es.Load(ctx, "job-2"); len(got) != 1 {
		t.Errorf("DeleteJob(job-1) removed job-2's extents")
	}
}

// TestSQLiteExtentStore_CommitErrorsOnClosedDB covers the BeginTx error
// path: a non-empty batch against a closed handle must return a wrapped
// error, not panic or silently drop the extents.
func TestSQLiteExtentStore_CommitErrorsOnClosedDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	es := NewSQLiteExtentStore(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	ext := FileExtent{FileIdx: 0, Durable: NewBitmap(8)}
	err := es.Commit(ctx, "job-1", []FileExtent{ext})
	if err == nil {
		t.Fatal("Commit on a closed db = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "durability: begin extent commit") {
		t.Errorf("Commit error = %q, want it to name the failing step", err)
	}
}

// TestSQLiteExtentStore_DeleteJobErrorsOnClosedDB covers DeleteJob's error
// path.
func TestSQLiteExtentStore_DeleteJobErrorsOnClosedDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	es := NewSQLiteExtentStore(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	err := es.DeleteJob(ctx, "job-1")
	if err == nil {
		t.Fatal("DeleteJob on a closed db = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "job=job-1") {
		t.Errorf("DeleteJob error = %q, want it to name the job", err)
	}
}

// TestSQLiteExtentStore_CommitEmptyIsNoop covers Commit's early return: an
// empty batch must not touch the database at all, so it succeeds even
// against an already-closed handle that would fail any real query.
func TestSQLiteExtentStore_CommitEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	es := NewSQLiteExtentStore(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := es.Commit(ctx, "job-1", nil); err != nil {
		t.Fatalf("Commit(nil) on a closed db = %v, want nil (should be a no-op)", err)
	}
}

// TestSQLiteExtentStore_LoadFileIsAPointLookup pins the three answers
// Barrier.priorExtent and Resumer.committedExtent depend on, and each of the
// three has a distinct consequence if it comes back wrong.
//
// The absent case is the one that matters most, and it is why the method
// reports a boolean rather than a zero value. A missing row is ordinary — no
// barrier has committed for the file yet — so returning an error would fail
// every first checkpoint. Reporting it as PRESENT would be worse: priorExtent
// would build on a zero-width bitmap and the commit would erase whatever
// durable bits the job had, which is the L3 loss the artCount guard in
// buildExtent exists to prevent.
//
// The wrong-file case is what the primary-key predicate buys. This method
// replaced a whole-job Load filtered per file, and a lookup that dropped the
// file_idx half of the key would return an arbitrary sibling's extent — a
// bitmap for the wrong file, acked against this one.
func TestSQLiteExtentStore_LoadFileIsAPointLookup(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))

	one := NewBitmap(8)
	one.Set(1)
	two := NewBitmap(8)
	two.Set(2)
	two.Set(5)
	if err := es.Commit(ctx, "job-1", []FileExtent{
		{FileIdx: 1, Durable: one, VerifiedTo: 100, BytesDurable: 100, Size: 100, ModTimeNs: 7},
		{FileIdx: 2, Durable: two, VerifiedTo: 200, PrefixCRC: 0xFEED, HasPrefixCRC: true,
			BytesDurable: 200, Size: 200, ModTimeNs: 9},
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := es.LoadFile(ctx, "job-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("LoadFile reported no row for a file that has one")
	}
	if got.FileIdx != 2 || got.VerifiedTo != 200 || got.PrefixCRC != 0xFEED || !got.HasPrefixCRC ||
		got.BytesDurable != 200 || got.Size != 200 || got.ModTimeNs != 9 {
		t.Errorf("LoadFile returned %+v, want file 2's own row — a lookup missing the "+
			"file_idx half of the key answers with a sibling's extent", got)
	}
	if !got.Durable.Get(2) || !got.Durable.Get(5) || got.Durable.Get(1) {
		t.Errorf("bitmap is file 1's, not file 2's: count=%d", got.Durable.Count())
	}

	// Absent: a file that exists in the job but has never been committed.
	if _, ok, err := es.LoadFile(ctx, "job-1", 3); err != nil || ok {
		t.Errorf("LoadFile for an uncommitted file = (ok=%v, err=%v), want (false, nil) — "+
			"an error fails every first checkpoint, and ok=true erases the job's durable bits", ok, err)
	}
	// Absent: scoped by job as well as by file.
	if _, ok, err := es.LoadFile(ctx, "job-2", 2); err != nil || ok {
		t.Errorf("LoadFile crossed a job boundary: (ok=%v, err=%v)", ok, err)
	}
}
