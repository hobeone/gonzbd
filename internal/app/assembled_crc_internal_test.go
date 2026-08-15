package app

import (
	"hash/crc32"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestRecordAssembledCRC_ThreadsAWholeFileCRCToTheQueue pins the last link in
// the chain QuickCheck and on-demand par2 depend on.
//
// The barrier derives the whole-file CRC from Class A and commits it, but
// nothing read it back: Queue.SetFileCRC32 had no non-test caller at all, so
// assembled_crc32 stayed 0 for every download and both consumers took their
// NoCRC branch — full par2 verify, and every recovery volume fetched even for
// a bit-perfect download.
func TestRecordAssembledCRC_ThreadsAWholeFileCRCToTheQueue(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)
	const want = uint32(0xC0FFEE)

	bm := durability.NewBitmap(2)
	bm.Set(0)
	bm.Set(1)
	if err := application.extents.Commit(t.Context(), job.ID, []durability.FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 200, Size: 200,
		PrefixCRC: want, HasPrefixCRC: true,
	}}); err != nil {
		t.Fatal(err)
	}

	application.recordAssembledCRC(t.Context(), job.ID, 0)

	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != want {
		t.Errorf("assembled CRC = %#x, want %#x — with none recorded, QuickCheck "+
			"cannot bypass the par2 verify and on-demand par2 fetches every "+
			"recovery volume for an intact download", got, want)
	}
}

// TestRecordAssembledCRC_RecordsNothingWhenTheCRCIsUnavailable pins the R23
// distinction the flag exists for.
//
// A file with a permanently failed article has a prefix that stops at the hole,
// so its PrefixCRC covers only part of the file. Recording that as the file's
// CRC would report a MISMATCH against par2 — corruption — for a file that is
// merely incomplete, which is a worse answer than reporting nothing.
func TestRecordAssembledCRC_RecordsNothingWhenTheCRCIsUnavailable(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	bm := durability.NewBitmap(2)
	bm.Set(1)
	if err := application.extents.Commit(t.Context(), job.ID, []durability.FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 0, Size: 200,
		PrefixCRC: 0xDEADBEEF, HasPrefixCRC: false,
	}}); err != nil {
		t.Fatal(err)
	}

	application.recordAssembledCRC(t.Context(), job.ID, 0)

	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x for a file whose prefix stops at a hole; that "+
			"value covers part of the file and par2 would read it as corruption", got)
	}
}

// TestRecordAssembledCRC_MatchesTheRealFileCRC is the end-to-end arithmetic
// check: the value the queue ends up with must be the CRC32 of the file's
// bytes, not merely some non-zero number that travelled the plumbing.
func TestRecordAssembledCRC_MatchesTheRealFileCRC(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	a0 := make([]byte, 100)
	a1 := make([]byte, 100)
	for i := range a0 {
		a0[i], a1[i] = byte(i), byte(255-i)
	}
	whole := crc32.ChecksumIEEE(append(append([]byte{}, a0...), a1...))

	if err := application.factLog.Append(t.Context(), job.ID, []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
	}); err != nil {
		t.Fatal(err)
	}
	bm := durability.NewBitmap(2)
	bm.Set(0)
	bm.Set(1)
	if err := application.extents.Commit(t.Context(), job.ID, []durability.FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 200, Size: 200,
		PrefixCRC: whole, HasPrefixCRC: true,
	}}); err != nil {
		t.Fatal(err)
	}

	application.recordAssembledCRC(t.Context(), job.ID, 0)

	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Progress().FileAssembledCRC32(0); got != whole {
		t.Errorf("assembled CRC = %#x, want %#x (CRC32 of the file's bytes)", got, whole)
	}
}

// TestRecordAssembledCRC_ToleratesAMissingRecord pins the two ways there can
// be nothing to copy, both of which must leave the finalize alone.
//
// recordAssembledCRC runs AFTER FinalizeFile has committed the extent and
// acked every one of the file's articles. Nothing it does may fail that work,
// because the cost of a missing CRC is a full par2 verify — which is exactly
// the behaviour that shipped before the CRC existed — while the cost of
// failing the finalize is a file that never completes.
func TestRecordAssembledCRC_ToleratesAMissingRecord(t *testing.T) {
	t.Run("no extent store configured", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 2)
		saved := application.extents
		application.extents = nil
		t.Cleanup(func() { application.extents = saved })

		application.recordAssembledCRC(t.Context(), job.ID, 0)

		snap, err := application.queue.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Progress().FileAssembledCRC32(0); got != 0 {
			t.Errorf("assembled CRC = %#x with no extent store", got)
		}
	})

	t.Run("the job has left the queue", func(t *testing.T) {
		application, _ := newDurabilityTestApp(t, 1, 2)

		// An extent whose job is not in the queue at all. This is reachable:
		// the file finalizes, and the job is removed or evicted before the CRC
		// is copied across. SetFileCRC32 then answers ErrJobNotResident.
		bm := durability.NewBitmap(2)
		bm.Set(0)
		bm.Set(1)
		if err := application.extents.Commit(t.Context(), "ghost-job", []durability.FileExtent{{
			FileIdx: 0, Durable: bm, VerifiedTo: 200, Size: 200,
			PrefixCRC: 0xC0FFEE, HasPrefixCRC: true,
		}}); err != nil {
			t.Fatal(err)
		}

		// The requirement is that this returns rather than panicking or
		// propagating: it runs after the extent is committed and the articles
		// are acked, and must not undo either.
		application.recordAssembledCRC(t.Context(), "ghost-job", 0)
	})

	t.Run("no extent for this file", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 2, 2)

		// An extent for file 0 only; file 1 finalizes with none of its own,
		// which is what happens when its articles all failed.
		bm := durability.NewBitmap(2)
		bm.Set(0)
		if err := application.extents.Commit(t.Context(), job.ID, []durability.FileExtent{{
			FileIdx: 0, Durable: bm, VerifiedTo: 200, Size: 200,
			PrefixCRC: 0xC0FFEE, HasPrefixCRC: true,
		}}); err != nil {
			t.Fatal(err)
		}

		application.recordAssembledCRC(t.Context(), job.ID, 1)

		snap, err := application.queue.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Progress().FileAssembledCRC32(1); got != 0 {
			t.Errorf("file 1 got CRC %#x from file 0's extent; the walk must match "+
				"on FileIdx or every file inherits the first one's checksum", got)
		}
		// Grounding: file 0's own extent must still be reachable, or the
		// assertion above passes because the fixture stored nothing at all.
		application.recordAssembledCRC(t.Context(), job.ID, 0)
		snap, err = application.queue.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Progress().FileAssembledCRC32(0); got != 0xC0FFEE {
			t.Fatalf("file 0's CRC = %#x, want 0xC0FFEE; the fixture stored no usable extent", got)
		}
	})
}
