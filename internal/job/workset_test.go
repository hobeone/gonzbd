package job

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// worksetJob builds a resident two-file job: file 0 holds articles 0-2 at 100
// bytes each, file 1 is a par2 recovery volume holding article 3 at 50.
//
// Two files, not one, because every operation here is bounded per FILE while
// the article indices it takes are GLOBAL — a single-file fixture makes the
// two numberings identical and would pass an implementation that confused
// them.
func worksetJob(t *testing.T) *Job {
	t.Helper()
	j := New("j1", "test", PolicyFromPP(3))
	m := NewManifest([]JobFile{
		{
			Subject: "big.rar", Bytes: 300,
			Articles: []JobArticle{
				{ID: "a0@x", Bytes: 100}, {ID: "a1@x", Bytes: 100}, {ID: "a2@x", Bytes: 100},
			},
		},
		{
			Subject: "big.vol000+01.par2", Bytes: 50, IsPar2Recovery: true,
			Articles: []JobArticle{{ID: "p0@x", Bytes: 50}},
		},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j
}

// run builds a durability.Run covering the given global article range of a
// file. The byte fields are not read by anything under test — runsCoverage
// checks the ARTICLE range against the file — so they are left zero rather
// than given plausible-looking values that would imply they mattered.
func run(fileIdx, first, last int32) durability.Run {
	return durability.Run{FileIdx: fileIdx, FirstArtIdx: first, LastArtIdx: last}
}

// TestAckDurable_EmptyProofAcksNothing pins the early return that makes the
// compile-time bound on DurableProof worth anything: an outside package CAN
// build durability.DurableProof{}, because Go permits a composite literal with
// no field values, so what stops it acking is that an empty payload is inert.
func TestAckDurable_EmptyProofAcksNothing(t *testing.T) {
	j := worksetJob(t)
	if err := j.AckDurable(durability.DurableProof{}); err != nil {
		t.Fatalf("AckDurable(empty) = %v, want nil", err)
	}
	if j.Progress().ArticlesResolved() != 0 {
		t.Fatalf("ArticlesResolved = %d, want 0", j.Progress().ArticlesResolved())
	}
}

// TestAckDurable_RefusesAProofForAnotherJob pins the check that replaced
// internal/queue's lookup. The Queue found the job the proof named; a *Job is
// chosen by the caller, so a mis-routed proof would otherwise mark articles
// done on a manifest that merely happens to be long enough.
func TestAckDurable_RefusesAProofForAnotherJob(t *testing.T) {
	j := worksetJob(t)
	err := j.ackDurable("someone-else", []int32{0, 1})
	if !errors.Is(err, ErrProofForAnotherJob) {
		t.Fatalf("ackDurable(other job) = %v, want ErrProofForAnotherJob", err)
	}
	if j.Progress().ArticlesResolved() != 0 {
		t.Fatalf("a refused proof marked %d articles, want 0", j.Progress().ArticlesResolved())
	}
}

// TestAckDurable_AppliesInRangeAndReportsTheRest pins both halves of the
// out-of-range contract: the in-range acks stand, because a real fsync earned
// them and discarding them costs a re-download, AND the anomaly is reported,
// because this package has no logger and A2 forbids dropping it silently.
func TestAckDurable_AppliesInRangeAndReportsTheRest(t *testing.T) {
	j := worksetJob(t)
	err := j.ackDurable("j1", []int32{0, 99, -1})
	if !errors.Is(err, ErrProofNamesUnknownArticles) {
		t.Fatalf("ackDurable(out of range) = %v, want ErrProofNamesUnknownArticles", err)
	}
	if !j.Progress().ArticleDone(0) {
		t.Fatal("article 0 was in range and must have been acked despite the error")
	}
	if got, want := j.Progress().FileBytesDownloaded(0), int64(100); got != want {
		t.Fatalf("FileBytesDownloaded(0) = %d, want %d", got, want)
	}
}

// TestAckDurable_IsIdempotent pins R12: Drain may re-report an article a
// previous Drain returned, so a replayed proof must not double-count bytes.
func TestAckDurable_IsIdempotent(t *testing.T) {
	j := worksetJob(t)
	for range 3 {
		if err := j.ackDurable("j1", []int32{0, 1}); err != nil {
			t.Fatalf("ackDurable: %v", err)
		}
	}
	if got, want := j.Progress().FileBytesDownloaded(0), int64(200); got != want {
		t.Fatalf("FileBytesDownloaded(0) after three replays = %d, want %d", got, want)
	}
	if got, want := j.Progress().ArticlesResolved(), 2; got != want {
		t.Fatalf("ArticlesResolved = %d, want %d", got, want)
	}
}

// TestAckDurable_RequiresAResidentManifest pins that the byte arithmetic
// cannot run without the manifest the article sizes live in — an evicted job
// reports rather than silently skipping the ack.
func TestAckDurable_RequiresAResidentManifest(t *testing.T) {
	j := worksetJob(t)
	j.Evict()
	if err := j.ackDurable("j1", []int32{0}); !errors.Is(err, ErrNotResident) {
		t.Fatalf("ackDurable on an evicted job = %v, want ErrNotResident", err)
	}
}

// TestAckPermanentFailure_ReportsWhatTheCallerMustPersistAndLog pins the shape
// that replaced internal/queue's inline store write and log lines: everything
// a caller needs comes back as a value, because this package does no I/O.
func TestAckPermanentFailure_ReportsWhatTheCallerMustPersistAndLog(t *testing.T) {
	j := worksetJob(t)
	got, err := j.AckPermanentFailure([]int32{0, 99})
	if err != nil {
		t.Fatalf("AckPermanentFailure: %v", err)
	}
	if len(got.Persist) != 1 || got.Persist[0] != 0 {
		t.Fatalf("Persist = %v, want [0]; an out-of-range index must never be persisted", got.Persist)
	}
	if got.FirstTime != 1 || got.Invalid != 1 || got.NumArticles != 4 {
		t.Fatalf("FirstTime/Invalid/NumArticles = %d/%d/%d, want 1/1/4",
			got.FirstTime, got.Invalid, got.NumArticles)
	}
	if got.FailedBytes != 100 || got.RecoveryBytes != 50 {
		t.Fatalf("FailedBytes/RecoveryBytes = %d/%d, want 100/50", got.FailedBytes, got.RecoveryBytes)
	}
	if !j.Progress().ArticleFailed(0) || !j.Progress().ArticleDone(0) {
		t.Fatal("a permanently failed article must be Done and Failed both")
	}
}

// TestAckPermanentFailure_IsANoOpTheSecondTime pins that FirstTime counts
// articles this call moved, not articles named: a caller logs damage from it,
// and a repeated ack must not report damage twice.
func TestAckPermanentFailure_IsANoOpTheSecondTime(t *testing.T) {
	j := worksetJob(t)
	if _, err := j.AckPermanentFailure([]int32{0}); err != nil {
		t.Fatalf("first AckPermanentFailure: %v", err)
	}
	got, err := j.AckPermanentFailure([]int32{0})
	if err != nil {
		t.Fatalf("second AckPermanentFailure: %v", err)
	}
	if got.FirstTime != 0 {
		t.Fatalf("FirstTime on the replay = %d, want 0", got.FirstTime)
	}
	if j.Progress().FailedBytes() != 100 {
		t.Fatalf("FailedBytes = %d, want 100 — a replay double-counted",
			j.Progress().FailedBytes())
	}
}

// TestAckPermanentFailure_ReleasesDeferredPar2 pins the on-demand par2 branch:
// a permanent data-article failure proves the job will need repair, so the
// held recovery volume is released now rather than at the completion verify.
func TestAckPermanentFailure_ReleasesDeferredPar2(t *testing.T) {
	j := worksetJob(t)
	// Defer the recovery volume the way an on-demand-par2 admission would.
	// Reached through the progress record directly because the door that does
	// this in production has not moved out of internal/queue yet.
	j.progress.files[1].Fetch = FetchIfNeeded

	got, err := j.AckPermanentFailure([]int32{0})
	if err != nil {
		t.Fatalf("AckPermanentFailure: %v", err)
	}
	if !got.ReleasedPar2 {
		t.Fatal("ReleasedPar2 = false, want true")
	}
	if j.Progress().FileFetchPolicy(1) != FetchAlways {
		t.Fatalf("recovery volume fetch policy = %v, want FetchAlways",
			j.Progress().FileFetchPolicy(1))
	}
	if !j.Progress().Par2Recovered() || !j.Progress().HasPar2Verdict() {
		t.Fatal("the release must set both par2Recovered and the reason")
	}
}

// TestAckPermanentFailure_LeavesADiscardedVolumeDiscarded pins
// DeferredRecoveryIndices' FetchIfNeeded-only rule from the consuming side: if
// a discarded volume were re-activated by one late failure, on-demand par2
// would be undone entirely.
func TestAckPermanentFailure_LeavesADiscardedVolumeDiscarded(t *testing.T) {
	j := worksetJob(t)
	j.progress.files[1].Fetch = FetchNever

	got, err := j.AckPermanentFailure([]int32{0})
	if err != nil {
		t.Fatalf("AckPermanentFailure: %v", err)
	}
	if got.ReleasedPar2 {
		t.Fatal("ReleasedPar2 = true; the CRC oracle had proved this volume unnecessary")
	}
	if j.Progress().FileFetchPolicy(1) != FetchNever {
		t.Fatalf("fetch policy = %v, want FetchNever", j.Progress().FileFetchPolicy(1))
	}
}

// TestAckPermanentFailure_EmptyAndNonResident covers the two refusals.
func TestAckPermanentFailure_EmptyAndNonResident(t *testing.T) {
	j := worksetJob(t)
	if got, err := j.AckPermanentFailure(nil); err != nil || got.FirstTime != 0 {
		t.Fatalf("AckPermanentFailure(nil) = %+v, %v; want a zero ack and no error", got, err)
	}
	j.Evict()
	if _, err := j.AckPermanentFailure([]int32{0}); !errors.Is(err, ErrNotResident) {
		t.Fatalf("AckPermanentFailure on an evicted job = %v, want ErrNotResident", err)
	}
}

// TestUndeferRecovery_IgnoresIndicesItWasNotAskedAbout exercises the helper
// directly, which its one production caller cannot: AckPermanentFailure passes
// DeferredRecoveryIndices, and that list is already FetchIfNeeded-only, so the
// skip below is unreachable from there. A mutation confirmed that — neutering
// the skip left every test through AckPermanentFailure green.
//
// The skip is kept rather than deleted because the arbitrary-index caller is
// real and has simply not moved out of internal/queue yet: the operator-facing
// door that un-defers named volumes passes indices it did not derive from the
// fetch policy. Without this test the guard would be live code nothing pins.
func TestUndeferRecovery_IgnoresIndicesItWasNotAskedAbout(t *testing.T) {
	j := worksetJob(t)
	j.progress.files[1].Fetch = FetchNever

	// Out of range, negative, and a volume the oracle already discarded.
	if j.progress.undeferRecovery(j.manifest, []int{7, -1, 1}) {
		t.Fatal("undeferRecovery reported a change; none of those indices is held")
	}
	if j.Progress().FileFetchPolicy(1) != FetchNever {
		t.Fatalf("fetch policy = %v, want FetchNever", j.Progress().FileFetchPolicy(1))
	}
	if j.Progress().Par2Recovered() {
		t.Fatal("par2Recovered was set by a call that changed nothing")
	}

	j.progress.files[1].Fetch = FetchIfNeeded
	if !j.progress.undeferRecovery(j.manifest, []int{7, 1}) {
		t.Fatal("undeferRecovery must report the one held volume it did release")
	}
	if j.Progress().FileFetchPolicy(1) != FetchAlways {
		t.Fatalf("fetch policy = %v, want FetchAlways", j.Progress().FileFetchPolicy(1))
	}
}

// TestSeedFromRuns_MarksEveryCoveredArticle pins the resume path L3 depends
// on: a restart must not re-fetch bytes an earlier run already got onto stable
// storage.
func TestSeedFromRuns_MarksEveryCoveredArticle(t *testing.T) {
	j := worksetJob(t)
	if err := j.SeedFromRuns([]durability.Run{run(0, 0, 1)}); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	if !j.Progress().ArticleDone(0) || !j.Progress().ArticleDone(1) {
		t.Fatal("articles 0 and 1 must be done")
	}
	if j.Progress().ArticleDone(2) {
		t.Fatal("article 2 was not covered and must be untouched")
	}
	if err := j.SeedFromRuns(nil); err != nil {
		t.Fatalf("SeedFromRuns(nil) = %v, want nil", err)
	}
}

// TestSeedFromRuns_StaysAdditive is the guard on the half of the pair that
// must never clear. Its caller is a stall recovery replaying runs loaded from
// the store, which has stat'ed nothing: a clear there discards the acks this
// process made since the last commit. It reddens if the two entry points are
// ever merged.
func TestSeedFromRuns_StaysAdditive(t *testing.T) {
	j := worksetJob(t)
	if err := j.ackDurable("j1", []int32{2}); err != nil {
		t.Fatalf("ackDurable: %v", err)
	}
	// A run set that says nothing about article 2.
	if err := j.SeedFromRuns([]durability.Run{run(0, 0, 0)}); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	if !j.Progress().ArticleDone(2) {
		t.Fatal("SeedFromRuns cleared an article no run named; it must only ever SET")
	}
}

// TestSeedFromRuns_AppliesNothingWhenAnyRunIsRefused pins validate-then-apply.
// A run runsCoverage refuses means the manifest was rebuilt under rows keyed
// on the old numbering, which is a statement about the whole batch: the runs
// ordered before it passed only because their indices happened to land in a
// valid range.
func TestSeedFromRuns_AppliesNothingWhenAnyRunIsRefused(t *testing.T) {
	j := worksetJob(t)
	runs := []durability.Run{
		run(0, 0, 1),
		run(1, 0, 0), // article 0 does not belong to file 1
	}
	if err := j.SeedFromRuns(runs); err == nil {
		t.Fatal("SeedFromRuns accepted a run outside its file's range")
	}
	if j.Progress().ArticlesResolved() != 0 {
		t.Fatalf("%d articles were marked by a refused batch, want 0",
			j.Progress().ArticlesResolved())
	}
}

// TestReplaceFromRuns_ClearsWhatTheSweepDisproved is #362: the resume stats a
// file, deletes the runs of one shorter than they claim, and those articles
// must go back to Outstanding. With only an additive door the earlier belief
// won and the file finalized over a zero-filled hole.
func TestReplaceFromRuns_ClearsWhatTheSweepDisproved(t *testing.T) {
	j := worksetJob(t)
	if err := j.SeedFromRuns([]durability.Run{run(0, 0, 2)}); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	j.progress.files[0].Complete = true
	j.progress.files[0].AssembledCRC32 = 0xdeadbeef

	// The sweep came back with a shorter run: only article 0 survives.
	cleared, err := j.ReplaceFromRuns([]int32{0}, []durability.Run{run(0, 0, 0)})
	if err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}
	if cleared != 2 {
		t.Fatalf("cleared = %d, want 2", cleared)
	}
	if !j.Progress().ArticleDone(0) {
		t.Fatal("article 0 was still covered and must stay done")
	}
	if j.Progress().ArticleDone(1) || j.Progress().ArticleDone(2) {
		t.Fatal("articles the surviving runs do not cover must return to Outstanding")
	}
	if j.Progress().FileComplete(0) || j.Progress().FileAssembledCRC32(0) != 0 {
		t.Fatal("a file that lost bytes is not that file any more: Complete and the CRC must clear")
	}
	// recompute ran, so the derived figures match the bitmaps rather than the
	// pre-clear state.
	if got, want := j.Progress().FileBytesDownloaded(0), int64(100); got != want {
		t.Fatalf("FileBytesDownloaded(0) = %d, want %d", got, want)
	}
}

// TestReplaceFromRuns_LeavesUnnamedFilesAndFailedArticlesAlone pins the two
// limits on the sweep's authority: an omitted file is silence rather than a
// finding of absence, and a permanently failed article's absence is the
// recorded outcome, not new information.
func TestReplaceFromRuns_LeavesUnnamedFilesAndFailedArticlesAlone(t *testing.T) {
	j := worksetJob(t)
	if err := j.SeedFromRuns([]durability.Run{run(1, 3, 3)}); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	if _, err := j.AckPermanentFailure([]int32{1}); err != nil {
		t.Fatalf("AckPermanentFailure: %v", err)
	}

	// File 1 is not named, and no run covers article 1.
	cleared, err := j.ReplaceFromRuns([]int32{0}, nil)
	if err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}
	if cleared != 0 {
		t.Fatalf("cleared = %d, want 0: nothing done in file 0 was clearable", cleared)
	}
	if !j.Progress().ArticleDone(3) {
		t.Fatal("file 1 was not named by the sweep and must keep its state entirely")
	}
	if !j.Progress().ArticleFailed(1) {
		t.Fatal("a permanently failed article must never be cleared")
	}
}

// TestReplaceFromRuns_RefusesABadIndexBeforeClearingAnything pins that a bad
// file index aborts the whole call rather than clearing the files that
// happened to be listed first, on the same reasoning SeedFromRuns validates
// its runs up front.
func TestReplaceFromRuns_RefusesABadIndexBeforeClearingAnything(t *testing.T) {
	j := worksetJob(t)
	if err := j.SeedFromRuns([]durability.Run{run(0, 0, 2)}); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	if _, err := j.ReplaceFromRuns([]int32{0, 7}, nil); err == nil {
		t.Fatal("ReplaceFromRuns accepted a file index the manifest does not have")
	}
	if !j.Progress().ArticleDone(0) {
		t.Fatal("a refused call cleared file 0's articles anyway")
	}
	if _, err := j.ReplaceFromRuns([]int32{0}, []durability.Run{run(1, 0, 0)}); err == nil {
		t.Fatal("ReplaceFromRuns accepted a run outside its file's range")
	}
}

// TestReplaceFromRuns_EmptyAndNonResident covers the two refusals. The
// residency one is the interesting half: internal/queue hydrated a paused job
// for the duration, and residency is internal/dispatch's to arrange now, so a
// caller that forgets must be told rather than silently skipped — skipping a
// paused job is exactly the branch #362 survived in.
func TestReplaceFromRuns_EmptyAndNonResident(t *testing.T) {
	j := worksetJob(t)
	if cleared, err := j.ReplaceFromRuns(nil, nil); err != nil || cleared != 0 {
		t.Fatalf("ReplaceFromRuns(nil) = %d, %v; want 0, nil", cleared, err)
	}
	j.Evict()
	if _, err := j.ReplaceFromRuns([]int32{0}, nil); !errors.Is(err, ErrNotResident) {
		t.Fatalf("ReplaceFromRuns on an evicted job = %v, want ErrNotResident", err)
	}
	if err := j.SeedFromRuns([]durability.Run{run(0, 0, 0)}); !errors.Is(err, ErrNotResident) {
		t.Fatalf("SeedFromRuns on an evicted job = %v, want ErrNotResident", err)
	}
}

// TestRunsCoverage_RefusesEveryWayARunCanLie exercises the checker directly,
// because its refusals are what stop a manifest rebuilt under an old numbering
// from silently marking arbitrary articles done.
func TestRunsCoverage_RefusesEveryWayARunCanLie(t *testing.T) {
	m := NewManifest([]JobFile{
		{Subject: "a", Articles: []JobArticle{{ID: "a0@x"}, {ID: "a1@x"}}},
		{Subject: "b", Articles: []JobArticle{{ID: "b0@x"}}},
	})
	for _, tc := range []struct {
		name string
		r    durability.Run
	}{
		{"file index negative", run(-1, 0, 0)},
		{"file index past the end", run(2, 0, 0)},
		{"first after last", run(0, 1, 0)},
		{"below the file's range", run(1, 0, 2)},
		{"above the file's range", run(0, 0, 2)},
	} {
		if _, _, err := runsCoverage(m, tc.r); err == nil {
			t.Errorf("%s: runsCoverage accepted %+v", tc.name, tc.r)
		}
	}
	first, last, err := runsCoverage(m, run(1, 2, 2))
	if err != nil || first != 2 || last != 2 {
		t.Fatalf("runsCoverage(valid) = %d, %d, %v; want 2, 2, nil", first, last, err)
	}
}
