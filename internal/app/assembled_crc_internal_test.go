package app

import (
	"context"
	"hash/crc32"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// recordRuns commits arts as durable runs for jobID.
func recordRuns(t *testing.T, application *Application, jobID string, arts ...durability.DurableArticle) {
	t.Helper()
	if err := application.runs.Commit(context.Background(), jobID, arts); err != nil {
		t.Fatalf("record runs: %v", err)
	}
}

// TestRecordAssembledCRC_ThreadsAWholeFileCRCToTheQueue pins the last link in
// the chain QuickCheck and on-demand par2 depend on.
//
// The barrier records a whole-file CRC as a side effect of merging — a file
// whose articles all abut collapses to one run at offset 0 whose crc32 IS the
// file's — but nothing read it back: Queue.SetFileCRC32 had no non-test caller
// at all, so assembled_crc32 stayed 0 for every download and both consumers
// took their NoCRC branch: full par2 verify, and every recovery volume fetched
// even for a bit-perfect download.
func TestRecordAssembledCRC_ThreadsAWholeFileCRCToTheQueue(t *testing.T) {
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

	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != want {
		t.Errorf("assembled CRC = %#x, want %#x (CRC32 of the file's bytes) — with none "+
			"recorded, QuickCheck cannot bypass the par2 verify and on-demand par2 "+
			"fetches every recovery volume for an intact download", got, want)
	}
}

// TestRecordAssembledCRC_RecordsNothingForAHoledFile pins the R23 distinction:
// "unavailable" must stay distinguishable from a CRC of zero.
//
// A file with a permanently failed article has a hole, a hole is a gap between
// runs, so the file keeps more than one row. Publishing the first row's crc32
// would report a MISMATCH against par2 — corruption — for a file that is
// merely incomplete, which is a worse answer than reporting nothing.
//
// This case is ALSO satisfied by the wrong predicate (see the overlap test
// below), which is exactly why pinning it alone is not enough.
func TestRecordAssembledCRC_RecordsNothingForAHoledFile(t *testing.T) {
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

	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x for a file with a hole in it; that value covers "+
			"part of the file and par2 would read it as corruption", got)
	}
}

// TestRecordAssembledCRC_RecordsNothingForAnOverlappedFile is #387, and it is
// the case the OTHER plausible predicate gets wrong.
//
// The rule is EXACTLY ONE ROW at offset 0 — a row COUNT. The tempting
// alternative is a SPAN: "some row starts at 0 and its length equals
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
// The row count is what carries prefixWalk.consumedAll's guarantee across.
// Without this test the holed-file case above passes under both predicates and
// nothing distinguishes them.
func TestRecordAssembledCRC_RecordsNothingForAnOverlappedFile(t *testing.T) {
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

	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
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
	application, job := newDurabilityTestApp(t, 1, 2)

	// Article 0 permanently failed, so the file's only run starts at 100.
	recordRuns(t, application, job.ID,
		durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x22},
	)

	application.recordAssembledCRC(t.Context(), job.ID, 0)

	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x from a single run starting at 100; it covers a "+
			"suffix of the file and par2 would read the difference as corruption", got)
	}
}

// TestRecordAssembledCRC_ToleratesAMissingRecord pins the three ways there can
// be nothing to copy, all of which must leave the finalize alone.
//
// recordAssembledCRC runs AFTER FinalizeFile has recorded the runs and acked
// every one of the file's articles. Nothing it does may fail that work,
// because the cost of a missing CRC is a full par2 verify — which is exactly
// the behaviour that shipped before the CRC existed — while the cost of
// failing the finalize is a file that never completes.
func TestRecordAssembledCRC_ToleratesAMissingRecord(t *testing.T) {
	t.Run("no run store configured", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 2)
		saved := application.runs
		application.runs = nil
		t.Cleanup(func() { application.runs = saved })

		application.recordAssembledCRC(t.Context(), job.ID, 0)

		snap, err := application.queue.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
			t.Errorf("assembled CRC = %#x with no run store", got)
		}
	})

	t.Run("the job has left the queue", func(t *testing.T) {
		application, _ := newDurabilityTestApp(t, 1, 2)

		// A run whose job is not in the queue at all. This is reachable: the
		// file finalizes, and the job is removed or evicted before the CRC is
		// copied across. SetFileCRC32 then answers ErrJobNotResident.
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

		// A run for file 0 only; file 1 finalizes with none of its own, which
		// is what happens when its articles all failed.
		recordRuns(t, application, job.ID,
			durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 200, CRC32: 0xC0FFEE},
		)

		application.recordAssembledCRC(t.Context(), job.ID, 1)

		snap, err := application.queue.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Progress().FileAssembledCRC32(1); got != 0 {
			t.Errorf("file 1 got CRC %#x from file 0's run; the query must be scoped to "+
				"the file or every file inherits the first one's checksum", got)
		}
		// Grounding: file 0's own run must still be reachable, or the
		// assertion above passes because the fixture stored nothing at all.
		application.recordAssembledCRC(t.Context(), job.ID, 0)
		snap, err = application.queue.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Progress().FileAssembledCRC32(0); got != 0xC0FFEE {
			t.Fatalf("file 0's CRC = %#x, want 0xC0FFEE; the fixture stored no usable run", got)
		}
	})
}
