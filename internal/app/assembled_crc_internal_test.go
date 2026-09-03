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
func TestRecordAssembledCRC_WithholdsWhenAnExactOffsetDuplicateWasDropped(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 3)

	cols := recordRunsReportingCollisions(t, application, job.ID(),
		durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1111},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 2, Offset: 0, Length: 100, CRC32: 0x3333},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x2222},
	)
	if len(cols) != 1 {
		t.Fatalf("Commit reported %d collisions, want 1: %+v", len(cols), cols)
	}
	if cols[0].Offset != 0 || cols[0].Kept != 0 || cols[0].Dropped != 2 {
		t.Errorf("collision = %+v, want offset 0 keeping article 0 and dropping article 2", cols[0])
	}

	runs, err := application.runs.ForFile(t.Context(), job.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Offset != 0 {
		t.Fatalf("stored runs = %+v; the fixture must leave exactly one run at offset 0", runs)
	}

	application.recordAssembledCRC(t.Context(), job.ID(), 0)

	j, ok := application.Dispatcher().Job(job.ID())
	if !ok || j == nil {
		t.Fatal("job not in dispatcher")
	}
	if got := j.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x, want 0", got)
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
func TestRecordAssembledCRC_ThreadsAWholeFileCRCToTheQueue(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	a0 := make([]byte, 100)
	a1 := make([]byte, 100)
	for i := range a0 {
		a0[i], a1[i] = byte(i), byte(255-i)
	}
	want := crc32.ChecksumIEEE(append(append([]byte{}, a0...), a1...))

	recordRuns(t, application, job.ID(),
		durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
	)

	application.recordAssembledCRC(t.Context(), job.ID(), 0)

	j, ok := application.Dispatcher().Job(job.ID())
	if !ok || j == nil {
		t.Fatal("job not in dispatcher")
	}
	if got := j.Progress().FileAssembledCRC32(0); got != want {
		t.Errorf("assembled CRC = %#x, want %#x (CRC32 of the file's bytes)", got, want)
	}
}

// TestRecordAssembledCRC_RecordsNothingForAHoledFile pins the R23 distinction:
// "unavailable" must stay distinguishable from a CRC of zero.
func TestRecordAssembledCRC_RecordsNothingForAHoledFile(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 3)

	recordRuns(t, application, job.ID(),
		durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x22},
	)

	runs, err := application.runs.ForFile(t.Context(), job.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("the fixture recorded %d runs, want 2", len(runs))
	}

	application.recordAssembledCRC(t.Context(), job.ID(), 0)

	j, ok := application.Dispatcher().Job(job.ID())
	if !ok || j == nil {
		t.Fatal("job not in dispatcher")
	}
	if got := j.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x for a file with a hole in it", got)
	}
}

// TestRecordAssembledCRC_RecordsNothingForAnOverlappedFile is #387, and it is
// the case the OTHER plausible predicate gets wrong.
func TestRecordAssembledCRC_RecordsNothingForAnOverlappedFile(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 3)

	recordRuns(t, application, job.ID(),
		durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x22},
		durability.DurableArticle{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50, CRC32: 0x33},
	)

	runs, err := application.runs.ForFile(t.Context(), job.ID(), 0)
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
		t.Fatalf("the merged run is [%d,%d) against a maximum of %d", runs[0].Offset, runs[0].Offset+runs[0].Length, maxEnd)
	}

	application.recordAssembledCRC(t.Context(), job.ID(), 0)

	j, ok := application.Dispatcher().Job(job.ID())
	if !ok || j == nil {
		t.Fatal("job not in dispatcher")
	}
	if got := j.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x for a file holding a displaced article's bytes", got)
	}
}

// TestRecordAssembledCRC_RecordsNothingWhenTheOnlyRunDoesNotStartAtZero pins
// the second half of the predicate.
func TestRecordAssembledCRC_RecordsNothingWhenTheOnlyRunDoesNotStartAtZero(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	recordRuns(t, application, job.ID(),
		durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x22},
	)

	application.recordAssembledCRC(t.Context(), job.ID(), 0)

	j, ok := application.Dispatcher().Job(job.ID())
	if !ok || j == nil {
		t.Fatal("job not in dispatcher")
	}
	if got := j.Progress().FileAssembledCRC32(0); got != 0 {
		t.Errorf("assembled CRC = %#x from a single run starting at 100", got)
	}
}

// TestRecordAssembledCRC_ToleratesAMissingRecord pins the four ways there can
// be nothing to copy, all of which must leave the finalize alone.
func TestRecordAssembledCRC_ToleratesAMissingRecord(t *testing.T) {
	t.Parallel()
	t.Run("no run store configured", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 2)
		saved := application.runs
		application.runs = nil
		t.Cleanup(func() { application.runs = saved })

		application.recordAssembledCRC(t.Context(), job.ID(), 0)

		j, ok := application.Dispatcher().Job(job.ID())
		if !ok || j == nil {
			t.Fatal("job not in dispatcher")
		}
		if got := j.Progress().FileAssembledCRC32(0); got != 0 {
			t.Errorf("assembled CRC = %#x with no run store", got)
		}
	})

	t.Run("the record cannot be read", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 2)

		saved := application.runs
		application.runs = failingRunStore{err: errors.New("database is locked")}
		t.Cleanup(func() { application.runs = saved })

		application.recordAssembledCRC(t.Context(), job.ID(), 0)

		j, ok := application.Dispatcher().Job(job.ID())
		if !ok || j == nil {
			t.Fatal("job not in dispatcher")
		}
		if got := j.Progress().FileAssembledCRC32(0); got != 0 {
			t.Errorf("assembled CRC = %#x from a store that returned an error", got)
		}
	})

	t.Run("the job has left the queue", func(t *testing.T) {
		application, _ := newDurabilityTestApp(t, 1, 2)

		recordRuns(t, application, "ghost-job",
			durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 200, CRC32: 0xC0FFEE},
		)

		application.recordAssembledCRC(t.Context(), "ghost-job", 0)
	})

	t.Run("no run for this file", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 2, 2)

		a0, a1 := []byte("first-half-of-file-zero"), []byte("second-half!")
		want := crc32.ChecksumIEEE(append(append([]byte{}, a0...), a1...))
		recordRuns(t, application, job.ID(),
			durability.DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: int32(len(a0)), CRC32: crc32.ChecksumIEEE(a0)},
			durability.DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: int64(len(a0)), Length: int32(len(a1)), CRC32: crc32.ChecksumIEEE(a1)},
		)

		application.recordAssembledCRC(t.Context(), job.ID(), 1)

		j, ok := application.Dispatcher().Job(job.ID())
		if !ok || j == nil {
			t.Fatal("job not in dispatcher")
		}
		if got := j.Progress().FileAssembledCRC32(1); got != 0 {
			t.Errorf("file 1 got CRC %#x from file 0's run", got)
		}

		application.recordAssembledCRC(t.Context(), job.ID(), 0)
		j, ok = application.Dispatcher().Job(job.ID())
		if !ok || j == nil {
			t.Fatal("job not in dispatcher")
		}
		if got := j.Progress().FileAssembledCRC32(0); got != want {
			t.Fatalf("file 0's CRC = %#x, want %#x", got, want)
		}
	})
}
