package app

import (
	"context"
	"errors"
	"hash/crc32"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// recordRuns commits arts as durable runs for jobID.
func recordRuns(t *testing.T, application *Application, jobID string, arts ...durability.DurableArticle) {
	t.Helper()
	if _, err := application.runs.Commit(context.Background(), jobID, arts); err != nil {
		t.Fatalf("record runs: %v", err)
	}
}

// TestRecordAssembledCRC_WithholdsWhenAnExactOffsetDuplicateWasDropped pins
// the #387 guard at the one shape that defeats every geometric form of it.
//
// Two articles claim offset 0. The store can keep only one — (job_id,
// file_idx, offset) is the primary key — so mergeAdjacentRuns drops the other,
// and what survives is a SINGLE run STARTING AT OFFSET 0. Every property a
// reader would reach for is satisfied: one row, offset zero, and a length
// equal to the file's size, because FinalizeFile derives its truncate bound
// from max(offset+length) over the same rows. Σ length equals the size too, so
// overlapFrom raises nothing either.
//
// Publishing there is #387 with the stakes raised. The value is a REAL CRC
// over the articles the record still holds, so it does not merely look wrong —
// par2.Assess compares it against the par2 MANIFEST and never opens the
// file (verifycrc.go), so it MATCHES, QuickCheckClean is set, and
// stage_repair.go returns without running par2 on a file whose bytes another
// article overwrote.
//
// What catches it is the article-coverage half of the predicate: the dropped
// article's index is in no run's span — no other article carries that index,
// and a merge extends a span only to LastArtIdx+1, so a span never contains an
// index no article contributed — and the single run therefore cannot cover the
// file's whole range. main enforced the same thing as prefixWalk.consumedAll.
func TestRecordAssembledCRC_WithholdsWhenAnExactOffsetDuplicateWasDropped(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 3)

	// Articles 0 and 2 both claim offset 0; article 1 abuts article 0. The
	// merge keeps article 0, drops article 2, and folds article 1 in — leaving
	// exactly one run at offset 0 covering articles [0,1] of a file whose
	// range is [0,2].
	cols := recordRunsReportingCollisions(t, application, job.ID,
		durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1111},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 2, Offset: 0, Length: 100, CRC32: 0x3333},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x2222},
	)
	if len(cols) != 1 {
		t.Fatalf("Commit reported %d collisions, want 1: %+v — without the report "+
			"the drop is invisible from the stored rows afterwards, and the user "+
			"is never told why an apparently healthy file needed repairing", len(cols), cols)
	}
	if cols[0].Offset != 0 || cols[0].Kept != 0 || cols[0].Dropped != 2 {
		t.Errorf("collision = %+v, want offset 0 keeping article 0 and dropping "+
			"article 2 — the longer entry is kept, and at equal length the lower "+
			"article index, so that the truncate bound never shrinks", cols[0])
	}

	// The fixture guard: this must be the ONE-ROW-AT-ZERO shape, or the test
	// passes through the row-count check and proves nothing about coverage.
	runs, err := application.runs.ForFile(t.Context(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Offset != 0 {
		t.Fatalf("stored runs = %+v; the fixture must leave exactly one run at "+
			"offset 0, otherwise the row-count half of the predicate withholds and "+
			"the coverage half is never reached", runs)
	}

	application.recordAssembledCRC(t.Context(), job.ID, 0)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("snap is nil")
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x, want 0 — the record does not account for "+
			"article 2, so no CRC over it describes this file. A published value "+
			"here matches par2's manifest, sets QuickCheckClean, and skips the "+
			"repair stage on a corrupted file (#387)", got)
	}
}

// recordRunsReportingCollisions commits arts and returns the collisions the
// store dropped, for the tests whose subject is the drop itself.
func recordRunsReportingCollisions(t *testing.T, application *Application, jobID string, arts ...durability.DurableArticle) []durability.Collision {
	t.Helper()
	cols, err := application.runs.Commit(context.Background(), jobID, arts)
	if err != nil {
		t.Fatalf("record runs: %v", err)
	}
	return cols
}

// TestRecordAssembledCRC_ThreadsAWholeFileCRCToTheQueue pins the last link in
// the chain QuickCheck and on-demand par2 depend on.
//
// The barrier records a whole-file CRC as a side effect of merging — a file
// whose articles all abut collapses to one run at offset 0 whose crc32 IS the
// file's — but nothing read it back: the queue's CRC setter had no non-test
// caller at all, so assembled_crc32 stayed 0 for every download and both consumers
// took their NoCRC branch: full par2 verify, and every recovery volume fetched
// even for a bit-perfect download.
func TestRecordAssembledCRC_ThreadsAWholeFileCRCToTheQueue(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	a0 := make([]byte, 100)
	a1 := make([]byte, 100)
	for i := range a0 {
		a0[i], a1[i] = byte(i), byte(255-i)
	}
	want := crc32.ChecksumIEEE(append(append([]byte{}, a0...), a1...))

	// The two articles abut in both offset and index, so the store merges them
	// into ONE run at offset 0 whose crc32 is the whole file's. The value is
	// never handed to the store directly — it is combined by the merge — so
	// this is the arithmetic check as well as the plumbing one.
	recordRuns(t, application, job.ID,
		durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
	)

	application.recordAssembledCRC(t.Context(), job.ID, 0)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("snap is nil")
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != want {
		t.Errorf("assembled CRC = %#x, want %#x (CRC32 of the file's bytes) — with none "+
			"recorded, the quickcheck stage cannot report Clean so repair runs the "+
			"full par2 verify, and on-demand par2 "+
			"fetches every recovery volume for an intact download", got, want)
	}
}

// TestRecordAssembledCRC_RecordsNothingForAHoledFile pins the R23 distinction:
// "unavailable" must stay distinguishable from a CRC of zero.
//
// A file with a permanently failed article INTERIOR to it — which is what the
// fixture below builds, articles 0 and 2 recorded and 1 missing — has a hole,
// a hole is a gap between runs, so the file keeps more than one row.
// Publishing the first row's crc32 would report a MISMATCH against par2 —
// corruption — for a file that is merely incomplete, which is a worse answer
// than reporting nothing.
//
// The interiority is the fixture's, and under the article-coverage condition
// it no longer changes the answer. A file whose LAST article failed leaves the
// survivors as one run at offset 0 — so the row count alone would publish a
// CRC over the trimmed short file — but that run does not cover the file's
// whole article range, so nothing is published there either. Both shapes reach
// the repair path by the NoCRC branch. The earlier behaviour, where the tail
// case published a value that then MISMATCHED par2, reached the same
// destination one branch later; what is gone is a published number describing
// bytes the file does not hold.
//
// This case is ALSO satisfied by the wrong predicate (see the overlap test
// below), which is exactly why pinning it alone is not enough.
func TestRecordAssembledCRC_RecordsNothingForAHoledFile(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 3)

	// Article 1 permanently failed: nothing covers [100,200).
	recordRuns(t, application, job.ID,
		durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x22},
	)

	// Grounding: the fixture must really have produced two rows, or the
	// assertion below holds for a reason unrelated to the predicate.
	runs, err := application.runs.ForFile(t.Context(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("the fixture recorded %d runs, want 2 — a hole must leave a gap between "+
			"rows for the row-count predicate to see", len(runs))
	}

	application.recordAssembledCRC(t.Context(), job.ID, 0)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("snap is nil")
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x for a file with a hole in it; that value covers "+
			"part of the file and par2 would read it as corruption", got)
	}
}

// TestRecordAssembledCRC_RecordsNothingForAnOverlappedFile is #387, and it is
// the case the OTHER plausible predicate gets wrong.
//
// The rule's first condition is EXACTLY ONE ROW at offset 0 — a row COUNT. The
// tempting alternative is a SPAN: "some row starts at 0 and its length equals
// max(offset+length)". It reads as the same rule and is not.
//
// The fixture is the difference. Articles 0 and 1 tile [0,200) into one merged
// row. Article 2 is displaced to [150,200): it abuts nothing, so it cannot
// merge, and it gets a row of its own. The merged row still starts at 0 and
// still spans the file's maximum — so the SPAN form publishes a CRC combined
// from the ORIGINAL articles while article 2's foreign bytes occupy 150-200.
// par2 then matches a manifest whose bytes are not on disk and never fetches
// the recovery volumes: the corruption is not merely undetected, it is
// unrepairable.
//
// The row count is what carries prefixWalk.consumedAll's guarantee across FOR
// THIS SHAPE. It does not carry it for an exact-offset duplicate, where the
// merge drops one of the pair and a single row survives — that is
// TestRecordAssembledCRC_WithholdsWhenAnExactOffsetDuplicateWasDropped's
// subject, and the article-coverage condition is what catches it. The two
// tests are the two halves of consumedAll; neither alone is the guarantee.
//
// Without this test the holed-file case above passes under both predicates and
// nothing distinguishes them.
func TestRecordAssembledCRC_RecordsNothingForAnOverlappedFile(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 3)

	recordRuns(t, application, job.ID,
		durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x22},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50, CRC32: 0x33},
	)

	// Grounding, and it is what makes this fixture exercise the span form
	// rather than merely being a second holed file: the merged row must reach
	// the file's maximum end offset, so a span check would be satisfied.
	runs, err := application.runs.ForFile(t.Context(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("the fixture recorded %d runs, want 2", len(runs))
	}
	var maxEnd int64
	for _, r := range runs {
		if end := r.Offset + r.Length; end > maxEnd {
			maxEnd = end
		}
	}
	if runs[0].Offset != 0 || runs[0].Offset+runs[0].Length != maxEnd {
		t.Fatalf("the merged run is [%d,%d) against a maximum of %d — a span check "+
			"would not be satisfied here, so this fixture cannot tell the two "+
			"predicates apart", runs[0].Offset, runs[0].Offset+runs[0].Length, maxEnd)
	}

	application.recordAssembledCRC(t.Context(), job.ID, 0)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("snap is nil")
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x for a file holding a displaced article's bytes "+
			"at [150,200). The value was combined from the articles that SHOULD be on "+
			"disk, so par2 matches it, skips the repair and never fetches the recovery "+
			"volumes — that is #387", got)
	}
}

// TestRecordAssembledCRC_RecordsNothingWhenTheOnlyRunDoesNotStartAtZero pins
// the second half of the predicate.
//
// One row is not enough on its own: a file whose only recorded run begins
// above 0 has bytes below it that nothing accounts for. Publishing that run's
// crc32 as the file's would describe a suffix while claiming to describe the
// whole.
func TestRecordAssembledCRC_RecordsNothingWhenTheOnlyRunDoesNotStartAtZero(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	// Article 0 permanently failed, so the file's only run starts at 100.
	recordRuns(t, application, job.ID,
		durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x22},
	)

	application.recordAssembledCRC(t.Context(), job.ID, 0)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("snap is nil")
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x from a single run starting at 100; it covers a "+
			"suffix of the file and par2 would read the difference as corruption", got)
	}
}

// TestRecordAssembledCRC_ToleratesAMissingRecord pins the four ways there can
// be nothing to copy, all of which must leave the finalize alone.
//
// recordAssembledCRC runs AFTER FinalizeFile has recorded the runs and acked
// every one of the file's articles. Nothing it does may fail that work,
// because the cost of a missing CRC is a full par2 verify — which is exactly
// the behaviour that shipped before the CRC existed — while the cost of
// failing the finalize is a file that never completes.
func TestRecordAssembledCRC_ToleratesAMissingRecord(t *testing.T) {
	t.Parallel()
	t.Run("no run store configured", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 2)
		saved := application.runs
		application.runs = nil
		t.Cleanup(func() { application.runs = saved })

		application.recordAssembledCRC(t.Context(), job.ID, 0)

		snap := application.queue.SnapshotJob(job.ID)
		if snap == nil {
			t.Fatal("snap is nil")
		}
		if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
			t.Errorf("assembled CRC = %#x with no run store", got)
		}
	})

	t.Run("the record cannot be read", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 2)

		// The runs are on stable storage and were committed before this call;
		// a read failure here is a transient database condition, not evidence
		// about the file. Swallowing it is what "best effort" means, and the
		// cost is one full par2 verify.
		saved := application.runs
		application.runs = failingRunStore{err: errors.New("database is locked")}
		t.Cleanup(func() { application.runs = saved })

		application.recordAssembledCRC(t.Context(), job.ID, 0)

		snap := application.queue.SnapshotJob(job.ID)
		if snap == nil {
			t.Fatal("snap is nil")
		}
		if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
			t.Errorf("assembled CRC = %#x from a store that returned an error; a "+
				"failed read must publish nothing rather than a stale or zero value "+
				"that par2 would compare against", got)
		}
	})

	t.Run("the job has left the queue", func(t *testing.T) {
		application, _ := newDurabilityTestApp(t, 1, 2)

		// A run whose job is not in the queue at all. This is reachable: the
		// file finalizes, and the job is removed or evicted before the CRC is
		// copied across. SetFileCRC32FromRuns then answers ErrJobNotResident.
		recordRuns(t, application, "ghost-job",
			durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 200, CRC32: 0xC0FFEE},
		)

		// The requirement is that this returns rather than panicking or
		// propagating: it runs after the runs are recorded and the articles
		// are acked, and must not undo either.
		application.recordAssembledCRC(t.Context(), "ghost-job", 0)
	})

	t.Run("no run for this file", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 2, 2)

		// Runs for file 0 only; file 1 finalizes with none of its own, which
		// is what happens when its articles all failed.
		//
		// BOTH of file 0's articles, not one covering the same bytes: the
		// grounding below needs file 0 to actually publish a CRC, and the
		// predicate withholds unless the record accounts for every article of
		// the file. A single 200-byte run leaves article 1 unaccounted for and
		// is exactly the shape
		// TestRecordAssembledCRC_WithholdsWhenAnExactOffsetDuplicateWasDropped
		// exists to reject.
		a0, a1 := []byte("first-half-of-file-zero"), []byte("second-half!")
		want := crc32.ChecksumIEEE(append(append([]byte{}, a0...), a1...))
		recordRuns(t, application, job.ID,
			durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: int32(len(a0)), CRC32: crc32.ChecksumIEEE(a0)},
			durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: int64(len(a0)), Length: int32(len(a1)), CRC32: crc32.ChecksumIEEE(a1)},
		)

		application.recordAssembledCRC(t.Context(), job.ID, 1)

		snap := application.queue.SnapshotJob(job.ID)
		if snap == nil {
			t.Fatal("snap is nil")
		}
		if got := snap.Progress().FileAssembledCRC32(1); got != 0 {
			t.Errorf("file 1 got CRC %#x from file 0's run; the query must be scoped to "+
				"the file or every file inherits the first one's checksum", got)
		}
		// Grounding: file 0's own run must still be reachable, or the
		// assertion above passes because the fixture stored nothing at all.
		application.recordAssembledCRC(t.Context(), job.ID, 0)
		snap = application.queue.SnapshotJob(job.ID)
		if snap == nil {
			t.Fatal("snap is nil")
		}
		if got := snap.Progress().FileAssembledCRC32(0); got != want {
			t.Fatalf("file 0's CRC = %#x, want %#x; the fixture stored no usable run", got, want)
		}
	})
}
