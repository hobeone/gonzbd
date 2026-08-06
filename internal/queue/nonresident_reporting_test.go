package queue

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// nonResidentJob returns a queue holding one job whose manifest has been
// evicted, which is the ordinary steady state for anything past maxActive.
func nonResidentJob(t *testing.T) (*Queue, *Job) {
	t.Helper()
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	filler := makeMultiFileJob(t, "nr-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJob(t, "nr-subject", 2, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: job is resident, so this exercises nothing")
	}
	if job.Progress() == nil {
		t.Fatal("fixture guard: progress must stay resident (docs/queue-lifecycle.md)")
	}
	return q, job
}

// The manifest tier: every one of these needs the manifest to resolve what
// it was asked to mutate, so there is no correct work to do without one.
// Each previously returned nil — reporting success for an operation it did
// not perform (#261). A caller that wants to treat non-residency as benign
// can still do so, but now by saying so with errors.Is rather than by being
// unable to tell.
func TestNonResident_ManifestTierReportsRatherThanSkips(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(q *Queue, id string) error
	}{
		{"MarkArticlesDone", func(q *Queue, id string) error {
			return q.MarkArticlesDone(id, []string{articleID(0, 0)})
		}},
		{"MarkArticleDone", func(q *Queue, id string) error {
			return q.MarkArticleDone(id, articleID(0, 0))
		}},
		{"MarkArticlesFailed", func(q *Queue, id string) error {
			_, err := q.MarkArticlesFailed(id, []string{articleID(0, 0)})
			return err
		}},
		{"MarkArticleFailed", func(q *Queue, id string) error {
			_, err := q.MarkArticleFailed(id, articleID(0, 0))
			return err
		}},
		{"MarkArticlesDoneByIdx", func(q *Queue, id string) error {
			return q.MarkArticlesDoneByIdx(id, []int32{0})
		}},
		{"MarkArticlesFailedByIdx", func(q *Queue, id string) error {
			_, err := q.MarkArticlesFailedByIdx(id, []int32{0})
			return err
		}},
		{"SetFileWriteCursor", func(q *Queue, id string) error {
			return q.SetFileWriteCursor(id, 0, 128)
		}},
		{"MarkFileComplete", func(q *Queue, id string) error {
			return q.MarkFileComplete(id, 0)
		}},
		{"SetFileFilename", func(q *Queue, id string) error {
			return q.SetFileFilename(id, 0, "out.rar")
		}},
		{"SetFileCRC32", func(q *Queue, id string) error {
			return q.SetFileCRC32(id, 0, 0xdeadbeef)
		}},
		{"UndeferRecoveryVolumes", func(q *Queue, id string) error {
			return q.UndeferRecoveryVolumes(id, []int{0})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, job := nonResidentJob(t)
			err := tc.call(q, job.ID)
			if err == nil {
				t.Fatalf("%s returned nil for a non-resident job: the caller cannot tell the work was skipped", tc.name)
			}
			if !errors.Is(err, ErrJobNotResident) {
				t.Errorf("%s = %v, want an error satisfying errors.Is(err, ErrJobNotResident)", tc.name, err)
			}
		})
	}
}

// A job that is genuinely absent must still report ErrNotFound, not
// ErrJobNotResident: "you are asking about nothing" and "this job's manifest
// is not loaded" are different answers and callers key off the difference.
func TestNonResident_AbsentJobStillReportsNotFound(t *testing.T) {
	q, _ := nonResidentJob(t)
	if err := q.MarkFileComplete("no-such-job", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkFileComplete on an absent job = %v, want ErrNotFound", err)
	}
	if err := q.SetFileCRC32("no-such-job", 0, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetFileCRC32 on an absent job = %v, want ErrNotFound", err)
	}
}

// The progress tier is the other half of #261, and it resolves the opposite
// way. JobProgress is permanently resident since the residency work, so these
// two have no reason to fail or skip: their guards are checking for a state
// that cannot occur, and SetPar2ReleaseReason additionally demanded a
// manifest it never reads. Both must simply do the work.
func TestNonResident_ProgressTierDoesTheWork(t *testing.T) {
	t.Run("SetPar2ReleaseReason", func(t *testing.T) {
		q, job := nonResidentJob(t)
		const reason = "damage detected in volume 3"
		if err := q.SetPar2ReleaseReason(job.ID, reason); err != nil {
			t.Fatalf("SetPar2ReleaseReason: %v", err)
		}
		if got := job.Progress().Par2ReleaseReason(); got != reason {
			t.Errorf("Par2ReleaseReason = %q, want %q: the reason lives in progress, which is always resident, so evicting the manifest must not discard it", got, reason)
		}
	})

	t.Run("RecordDownload", func(t *testing.T) {
		q, job := nonResidentJob(t)
		if err := q.RecordDownload(job.ID, "news.example.com", 4096); err != nil {
			t.Fatalf("RecordDownload: %v", err)
		}
		if got := job.Progress().ServerStats()["news.example.com"]; got != 4096 {
			t.Errorf("ServerStats[news.example.com] = %d, want 4096: per-server byte counts live in progress and must survive manifest eviction", got)
		}
	})

	// DiscardDeferredPar2 moved into this tier along with the rest of Task
	// 2: it reads only job.progress.files, so an evicted manifest must not
	// block it. Left in the manifest tier, an on-demand-par2 job that
	// exceeded MaxActiveJobs and verified clean would report
	// ErrJobNotResident, leave its recovery volumes FetchIfNeeded, and force
	// maybeReleaseRecoveryVolumes to redo full CRC verification on every
	// later completion event instead of trusting the verdict it already
	// reached.
	t.Run("DiscardDeferredPar2", func(t *testing.T) {
		store, dir := setupResidencyTestStore(t)
		q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

		filler := makeMultiFileJob(t, "nr-par2-filler", 1, 1)
		if err := q.Add(filler); err != nil {
			t.Fatalf("Add filler: %v", err)
		}
		job, err := NewJob(par2NZB(), AddOptions{Filename: "nr-par2.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("NewJob: %v", err)
		}
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if manifestResident(job) {
			t.Fatal("fixture guard: job is resident, so this exercises nothing")
		}
		if !job.HasDeferredPar2() {
			t.Fatal("fixture guard: no deferred par2 volume, so there is nothing to discard")
		}

		if err := q.DiscardDeferredPar2(job.ID); err != nil {
			t.Fatalf("DiscardDeferredPar2 on a non-resident job = %v, want it to work from progress alone", err)
		}
		if job.HasDeferredPar2() {
			t.Error("the recovery volume is still deferred after a discard on a non-resident job")
		}
		if got := job.Progress().FileFetchPolicy(2); got != FetchNever {
			t.Errorf("file 2 policy = %d, want FetchNever", got)
		}
		if manifestResident(job) {
			t.Error("DiscardDeferredPar2 made the job resident; it should have needed nothing but progress")
		}
	})
}

// MarkArticlesFailedByIdx's actual working paths, which the residency
// conversion above pulled into the function-scoped coverage gate and found
// largely unexercised from inside this package: the empty-input short
// circuit, the out-of-range tally, a real first-time failure, and the
// early par2 release that a permanent failure triggers.
func TestMarkArticlesFailedByIdx_WorkingPaths(t *testing.T) {
	t.Run("empty input does nothing", func(t *testing.T) {
		q, job := nonResidentJob(t)
		// Deliberately a non-resident job: the length check precedes the
		// residency lookup, so an empty batch is not an error even for a job
		// that could not have serviced a non-empty one.
		got, err := q.MarkArticlesFailedByIdx(job.ID, nil)
		if err != nil || got != nil {
			t.Errorf("MarkArticlesFailedByIdx(nil) = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("out-of-range indices are tallied, not applied", func(t *testing.T) {
		q := New(WithMaxActiveJobs(4))
		job := makeMultiFileJob(t, "mafbi-range", 1, 2)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		nArt := job.NumArticles()
		got, err := q.MarkArticlesFailedByIdx(job.ID, []int32{-1, int32(nArt), int32(nArt + 100)})
		if err != nil {
			t.Fatalf("MarkArticlesFailedByIdx: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("out-of-range indices reported %d first-time failures, want 0", len(got))
		}
		for i := range nArt {
			if job.Progress().ArticleFailed(i) {
				t.Errorf("article %d was marked failed by an out-of-range request", i)
			}
		}
	})

	t.Run("first-time failures are reported once", func(t *testing.T) {
		q := New(WithMaxActiveJobs(4))
		job := makeMultiFileJob(t, "mafbi-first", 1, 2)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		got, err := q.MarkArticlesFailedByIdx(job.ID, []int32{0, 1})
		if err != nil {
			t.Fatalf("MarkArticlesFailedByIdx: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("first call reported %d first-time failures, want 2", len(got))
		}
		if !job.Progress().ArticleFailed(0) || !job.Progress().ArticleFailed(1) {
			t.Error("articles are not marked failed after a successful call")
		}
		if fb := job.Progress().FailedBytes(); fb <= 0 {
			t.Errorf("FailedBytes = %d after two failures, want > 0", fb)
		}
		// The return value is "newly failed", not "failed": re-reporting the
		// same articles must not double-count them into failed bytes.
		before := job.Progress().FailedBytes()
		again, err := q.MarkArticlesFailedByIdx(job.ID, []int32{0, 1})
		if err != nil {
			t.Fatalf("second MarkArticlesFailedByIdx: %v", err)
		}
		if len(again) != 0 {
			t.Errorf("re-reporting the same articles returned %d as first-time, want 0", len(again))
		}
		if after := job.Progress().FailedBytes(); after != before {
			t.Errorf("FailedBytes moved from %d to %d on a repeat report", before, after)
		}
	})

	t.Run("a permanent failure releases deferred par2 volumes", func(t *testing.T) {
		q := New(WithMaxActiveJobs(4))
		// OnDemandPar2 is what defers recovery volumes at add time; without
		// it there is nothing held back and this path cannot run.
		job, err := NewJob(par2NZB(), AddOptions{Filename: "mafbi-par2.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("NewJob: %v", err)
		}
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if !job.HasDeferredPar2() {
			t.Fatal("fixture guard: no par2 volume is deferred, so the release path below cannot be reached")
		}
		if _, mErr := q.MarkArticlesFailedByIdx(job.ID, []int32{0}); mErr != nil {
			t.Fatalf("MarkArticlesFailedByIdx: %v", mErr)
		}
		if job.HasDeferredPar2() {
			t.Error("par2 volumes are still deferred after a permanent article failure; recovery data will never be fetched")
		}
		if reason := job.Progress().Par2ReleaseReason(); reason == "" {
			t.Error("par2 volumes were released without recording why")
		}
	})
}
