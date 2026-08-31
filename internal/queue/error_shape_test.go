package queue

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestManifestTierErrorShapes pins the exact error strings the manifest-tier
// Queue methods produce, because B2.4a Task 3 splits where they are built.
//
// Before the split, one call to Queue.residentJob produced the residency
// error and the method body produced the index error, both naming the job. If
// the wrapper simply re-wrapped whatever the moved Job method returned, the
// index errors would gain a second job ID —
//
//	queue: fileIdx 9 out of range for job j1: j1
//
// — which is why the ID is added exactly once, and by the moved Job method
// rather than by the wrapper. Each one opens with
//
//	if err := j.resident(); err != nil { return fmt.Errorf("%w: %s", err, j.ID) }
//
// and builds its index errors with j.ID the same way, so both kinds reach the
// wrapper already named and the wrapper returns them untouched. An earlier
// draft of this comment put the residency wrapping in the wrapper; that shape
// yields the same strings, which is precisely why it is worth stating which
// one was built. The distinction is invisible in the types and survives only
// if something asserts the strings.
//
// This test must pass BEFORE Task 3 and after it. One that only passes
// afterwards is describing the new behaviour, not preserving the old.
//
// The AckDurable/SeedFromRuns rows look wrong and are not: those two wrap the
// residency error a second time with their own prefix, so the job ID legitimately
// appears TWICE today. Preserving that is the point — this test would have to
// change for a fix, which is what makes it a pin rather than an aspiration.
func TestManifestTierErrorShapes(t *testing.T) {
	t.Parallel()

	t.Run("residency errors name the job once, or twice where they already did", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			call func(q *Queue, id string) error
			want func(id string) string
		}{
			{"MarkFileComplete", func(q *Queue, id string) error { return q.MarkFileComplete(id, 0) },
				func(id string) string { return "queue: job not resident: " + id }},
			{"SetFileFilename", func(q *Queue, id string) error { return q.SetFileFilename(id, 0, "x") },
				func(id string) string { return "queue: job not resident: " + id }},
			{"MarkArticleEmittedByIdx", func(q *Queue, id string) error { return q.MarkArticleEmittedByIdx(id, 0) },
				func(id string) string { return "queue: job not resident: " + id }},
			{"CountUnfinishedArticles", func(q *Queue, id string) error {
				_, err := q.CountUnfinishedArticles(id, 0)
				return err
			}, func(id string) string { return "queue: job not resident: " + id }},
			// Wraps a second time with its own prefix: the ID appears twice.
			// The run must be non-empty or SeedFromRuns returns nil before it
			// ever reaches the residency check.
			{"SeedFromRuns", func(q *Queue, id string) error {
				return q.SeedFromRuns(id, []durability.Run{{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Length: 1}})
			}, func(id string) string {
				return fmt.Sprintf("queue: SeedFromRuns %s: queue: job not resident: %s", id, id)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				q, job := nonResidentJob(t)
				err := tc.call(q, job.ID)
				if !errors.Is(err, ErrJobNotResident) {
					t.Fatalf("err = %v, want ErrJobNotResident", err)
				}
				if got, want := err.Error(), tc.want(job.ID); got != want {
					t.Errorf("err = %q\nwant %q", got, want)
				}
			})
		}
	})

	t.Run("index errors already name the job and must not be re-wrapped", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "shape-oob", 2, 2)
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
		nFiles := mustManifest(t, j).NumFiles()
		nArts := mustManifest(t, j).NumArticles()

		for _, tc := range []struct {
			name string
			err  error
			want string
		}{
			{"MarkFileComplete", q.MarkFileComplete(j.ID, nFiles),
				fmt.Sprintf("queue: fileIdx %d out of range for job %s", nFiles, j.ID)},
			{"SetFileFilename", q.SetFileFilename(j.ID, nFiles, "x"),
				fmt.Sprintf("queue: fileIdx %d out of range for job %s", nFiles, j.ID)},
			{"SetFileCRC32FromRuns", q.SetFileCRC32FromRuns(j.ID, nFiles, nil),
				fmt.Sprintf("queue: fileIdx %d out of range for job %s", nFiles, j.ID)},
			{"CountUnfinishedArticles", countErr(q, j.ID, nFiles),
				fmt.Sprintf("queue: fileIdx %d out of range for job %s", nFiles, j.ID)},
			{"MarkArticleEmittedByIdx", q.MarkArticleEmittedByIdx(j.ID, int32(nArts)),
				fmt.Sprintf("queue: artIdx %d out of range for job %s", nArts, j.ID)},
			{"ClearArticleEmittedByIdx", q.ClearArticleEmittedByIdx(j.ID, int32(nArts)),
				fmt.Sprintf("queue: artIdx %d out of range for job %s", nArts, j.ID)},
		} {
			if tc.err == nil {
				t.Errorf("%s: err = nil, want an out-of-range error", tc.name)
				continue
			}
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("%s: err = %q\nwant %q", tc.name, got, tc.want)
			}
		}
	})

	// The two prefixing methods build their not-found error by hand since
	// B2.4a: the wrapper no longer calls residentJob, so it composes
	// "queue: <method> %s: %w: %s" itself. That reconstruction is the single
	// most error-prone line in the change and had no test — lifecycle_test.go
	// only asserts SeedFromRuns("bogus") returns non-nil. At origin/main the
	// string came from residentJob's "%w: %s" wrapped a second time by the
	// caller, so the ID appears twice; getting it wrong here would be silent.
	t.Run("the prefixing methods name a missing job twice, as they always did", func(t *testing.T) {
		t.Parallel()
		q := New()

		seedErr := q.SeedFromRuns("bogus", []durability.Run{{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Length: 1}})
		if !errors.Is(seedErr, ErrNotFound) {
			t.Fatalf("SeedFromRuns err = %v, want ErrNotFound", seedErr)
		}
		if got, want := seedErr.Error(), "queue: SeedFromRuns bogus: queue: job not found: bogus"; got != want {
			t.Errorf("SeedFromRuns err = %q\nwant %q", got, want)
		}

		ackErr := q.AckDurable(mintProof(t, "bogus", []int32{0}))
		if !errors.Is(ackErr, ErrNotFound) {
			t.Fatalf("AckDurable err = %v, want ErrNotFound", ackErr)
		}
		if got, want := ackErr.Error(), "queue: AckDurable bogus: queue: job not found: bogus"; got != want {
			t.Errorf("AckDurable err = %q\nwant %q", got, want)
		}
	})

	t.Run("RecordDownload returns the bare sentinel, unlike its neighbours", func(t *testing.T) {
		t.Parallel()
		q := New()
		err := q.RecordDownload("nope", "srv", 1)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		// Its neighbours return "queue: job not found: nope". This one does
		// not, and matching them would be a behaviour change disguised as
		// consistency.
		if got, want := err.Error(), ErrNotFound.Error(); got != want {
			t.Errorf("err = %q, want the unwrapped %q", got, want)
		}
	})
}

func countErr(q *Queue, id string, fileIdx int) error {
	_, err := q.CountUnfinishedArticles(id, fileIdx)
	return err
}
