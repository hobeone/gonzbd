package durability

import (
	"context"
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
		BytesDurable: 3000, BytesFailed: 120, Size: 8192, ModTimeNs: 1_700_000_000_000_000_000,
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
	if g.BytesDurable != 3000 || g.BytesFailed != 120 || g.Size != 8192 || g.ModTimeNs != 1_700_000_000_000_000_000 {
		t.Errorf("cache/stamp round trip wrong: %+v", g)
	}
	if g.Durable.Count() != 3 || !g.Durable.Get(0) || !g.Durable.Get(42) || !g.Durable.Get(99) {
		t.Errorf("bitmap round trip wrong: count=%d", g.Durable.Count())
	}
}

// TestSQLiteExtentStore_CommitIsAtomic pins that a job's files are never
// observable half-committed. A barrier that fails partway must leave the
// previous committed cache intact (R7).
// TestSQLiteExtentStore_HasPrefixCRCRoundTrips pins the same distinction
// TestSQLiteFactLog_HasCRCRoundTrips pins for Class A: a prefix CRC that is
// genuinely zero must stay distinguishable from no prefix CRC at all.
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
	if err := es.Commit(ctx, "job-1", bad); err == nil {
		t.Fatal("Commit accepted a negative VerifiedTo")
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].VerifiedTo != 100 {
		t.Fatalf("VerifiedTo = %d after a failed commit, want 100 — the batch was not atomic", got[0].VerifiedTo)
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
	if err := es.Commit(ctx, "job-1", []FileExtent{ext}); err == nil {
		t.Fatal("Commit on a closed db = nil error, want an error")
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
	if err := es.DeleteJob(ctx, "job-1"); err == nil {
		t.Fatal("DeleteJob on a closed db = nil error, want an error")
	}
}
