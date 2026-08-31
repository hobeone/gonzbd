package queue

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestJobArticleMethods_GateOnResidency drives every moved manifest-tier
// method directly against a job whose manifest is absent.
//
// TestManifestAccessIsGated proves each of these CALLS j.resident(); it cannot
// prove any of them RETURNS its error, because the AST walk sees the call and
// not what happens to the result. A method that called the gate and then
// ignored it would pass that test and dereference nil here.
//
// Driving the *Job methods directly rather than through their Queue wrappers
// is the point: B2.4a₂ exports these, and from then on a caller can hold a
// *Job and reach them without a wrapper's lookup in front. This is the
// contract that has to hold when it does.
func TestJobArticleMethods_GateOnResidency(t *testing.T) {
	t.Parallel()

	// A job with progress but no manifest: the ordinary steady state for
	// anything past maxActive, not a contrived one (docs/queue-lifecycle.md).
	newEvicted := func() *Job { return &Job{ID: "j1", progress: &JobProgress{}} }

	for _, tc := range []struct {
		name string
		call func(j *Job) error
	}{
		{"countUnfinishedArticles", func(j *Job) error { _, err := j.countUnfinishedArticles(0); return err }},
		{"markArticleEmittedByIdx", func(j *Job) error { return j.markArticleEmittedByIdx(0) }},
		{"clearArticleEmittedByIdx", func(j *Job) error { return j.clearArticleEmittedByIdx(0) }},
		{"markFileComplete", func(j *Job) error { return j.markFileComplete(0) }},
		{"setFileFilename", func(j *Job) error { return j.setFileFilename(0, "x") }},
		{"setFileCRC32FromRuns", func(j *Job) error { _, err := j.setFileCRC32FromRuns(0, nil); return err }},
		{"checkFileIdxs", func(j *Job) error { return j.checkFileIdxs([]int{0}) }},
		{"ackDurable", func(j *Job) error { _, _, err := j.ackDurable([]int32{0}); return err }},
		{"seedFromRuns", func(j *Job) error {
			return j.seedFromRuns([]durability.Run{{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Length: 1}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call(newEvicted())
			if !errors.Is(err, ErrJobNotResident) {
				t.Fatalf("on a non-resident job = %v, want ErrJobNotResident", err)
			}
			// The Job methods build the complete error, including the ID; the
			// wrappers pass it through rather than re-wrapping. See
			// TestManifestTierErrorShapes for why that split matters.
			if got, want := err.Error(), "queue: job not resident: j1"; got != want {
				t.Errorf("err = %q, want %q", got, want)
			}
		})
	}
}

// TestJobUndeferRecovery_IsFalseWhenNotResident covers the one moved method
// with no error channel.
//
// undeferRecovery returns a bare bool, so it cannot report non-residency and
// must instead answer "nothing changed" — which is true, and is the same
// answer the caller acts on. Reporting true would make
// Queue.UndeferRecoveryVolumes mark the queue dirty and wake the dispatcher
// for a job it did not touch.
func TestJobUndeferRecovery_IsFalseWhenNotResident(t *testing.T) {
	t.Parallel()
	j := &Job{ID: "j1", progress: &JobProgress{}}
	if j.undeferRecovery([]int{0}) {
		t.Error("undeferRecovery on a non-resident job reported a change; the caller would dirty the queue and notify for nothing")
	}
}

// TestJobSetFileCRC32FromRuns_RefusesRunsThatAreNotEvidence pins the predicate
// that gives this setter its shape.
//
// It takes the RUNS rather than a uint32 so it can refuse a value that is not
// evidence for the whole file. A run set that is not exactly one run, starting
// at offset zero, spanning the file's entire article range, proves nothing
// about the assembled bytes — so it records nothing and reports no change,
// which is not an error. The bool is what stops the wrapper marking the queue
// dirty for a write that did not happen.
func TestJobSetFileCRC32FromRuns_RefusesRunsThatAreNotEvidence(t *testing.T) {
	t.Parallel()

	j := makeMultiFileJob(t, "crc-evidence", 1, 3)
	m := mustManifest(t, j)
	lo, hi := m.FileRange(0)

	covering := durability.Run{
		FileIdx: 0, Offset: 0,
		FirstArtIdx: int32(lo), LastArtIdx: int32(hi - 1),
		CRC32: 0xC0FFEE,
	}

	for _, tc := range []struct {
		name string
		runs []durability.Run
	}{
		{"no runs", nil},
		{"two runs", []durability.Run{covering, covering}},
		{"does not start at offset 0", []durability.Run{withOffset(covering, 1)}},
		{"does not start at the file's first article", []durability.Run{withFirst(covering, int32(lo+1))}},
		{"does not reach the file's last article", []durability.Run{withLast(covering, int32(hi-2))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := makeMultiFileJob(t, "crc-"+tc.name, 1, 3)
			stored, err := probe.setFileCRC32FromRuns(0, tc.runs)
			if err != nil {
				t.Fatalf("err = %v, want nil — non-evidence is not an error", err)
			}
			if stored {
				t.Error("reported a store for runs that are not evidence for the whole file")
			}
			if got := probe.progress.files[0].AssembledCRC32; got != 0 {
				t.Errorf("AssembledCRC32 = %#x, want 0", got)
			}
		})
	}

	t.Run("a covering run is stored", func(t *testing.T) {
		t.Parallel()
		stored, err := j.setFileCRC32FromRuns(0, []durability.Run{covering})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !stored {
			t.Fatal("reported no store for a run covering the whole file")
		}
		if got := j.progress.files[0].AssembledCRC32; got != 0xC0FFEE {
			t.Errorf("AssembledCRC32 = %#x, want 0xc0ffee", got)
		}
	})
}

func withOffset(r durability.Run, off int64) durability.Run { r.Offset = off; return r }
func withFirst(r durability.Run, i int32) durability.Run    { r.FirstArtIdx = i; return r }
func withLast(r durability.Run, i int32) durability.Run     { r.LastArtIdx = i; return r }
