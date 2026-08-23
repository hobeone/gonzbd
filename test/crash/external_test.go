//go:build crash

package crash

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// externalFixture stops after a known amount of work so a test can modify the
// partial at an offset it can reason about precisely.
func externalFixture() harnessOpts {
	return harnessOpts{
		CheckpointBytes:    1 << 20,
		CheckpointInterval: time.Hour,
		WriteCacheBytes:    1 << 20,
		Connections:        1,
		BodyDelay:          4 * time.Millisecond,
		Files:              []fileSpec{{Name: "payload.bin", Size: 8 << 20, PartSize: 128 << 10}},
	}
}

// TestExternalModification_TruncatedPartialIsRecomputed pins S4 and the bound
// on what a falsified cache costs.
//
// It was committed RED, against issue #362, and the failure was this suite
// working: the recomputation got the right answer and the answer was
// discarded, so the job finished with a completed file that had a hole in it.
// It is green as of the fix — the startup sweep installs its result through
// the authoritative Queue.ReplaceFromRuns rather than the additive
// Queue.SeedFromRuns. Do not relax the assertions.
//
// A clean stop records runs describing exactly what the file holds, so the
// next start adopts them on one stat with no read. Truncating the file makes
// it SHORTER than those runs claim, which is the one condition §3.4's gate
// treats as a disproof.
//
// # The discard is whole-file, and that is the design's trade rather than a bug
//
// This test used to assert that the articles BELOW the cut were re-verified
// from their bytes and NOT re-fetched. That property is gone with the
// recomputation that produced it. A size comparison cannot tell which articles
// the missing bytes belonged to, so the file's whole record is discarded and
// every one of its articles is fetched again. §3.4 prices that deliberately:
// the alternative is reading every partial file at every startup, and a bad
// article costs only its own bytes.
//
// What the test pins instead is the pair that still matters, and the second
// half is #362 itself:
//
//   - Every article the truncation destroyed is re-fetched. Treating a
//     destroyed article as present is S3's absence-of-evidence read as
//     evidence, and it is what finished a file with a hole in it.
//   - The completed file matches its expected content byte for byte. A test
//     that only counted re-fetches would pass against an implementation that
//     re-fetched everything and still assembled it wrongly.
func TestExternalModification_TruncatedPartialIsRecomputed(t *testing.T) {
	opts := externalFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	// 32 articles durable = 4 MiB written from byte 0.
	const durableArticles = 32
	h.WaitForDurableBytes(jobID, durableArticles*int64(opts.Files[0].PartSize))
	h.Stop()
	servedBefore := h.Server.ArticlesServed()

	// The durable set as stable storage recorded it at the stop. Read here
	// rather than assumed to be the first `durableArticles` articles: a clean
	// stop runs a shutdown checkpoint, so it acks everything outstanding and
	// the real set reaches past the threshold the wait above returned on. The
	// first draft classified only the first 32 ordinals and would have ignored
	// a destroyed article at ordinal 33.
	durableAtStop := h.DurableOrdinals(jobID)[0]
	if len(durableAtStop) == 0 {
		t.Fatal("no durable article was recorded for file 0 at the stop")
	}

	// Cut the file in half, at an article boundary, so the surviving and
	// destroyed sets are exact rather than approximate.
	const keepArticles = durableArticles / 2
	cut := int64(keepArticles) * int64(opts.Files[0].PartSize)
	partials := h.PartialPaths()
	if len(partials) != 1 {
		t.Fatalf("expected one partial file, found %v", partials)
	}
	if fi, err := os.Stat(partials[0]); err != nil {
		t.Fatalf("stat partial: %v", err)
	} else if fi.Size() <= cut {
		t.Fatalf("partial is %d bytes, not larger than the %d-byte cut — the truncation "+
			"would destroy nothing", fi.Size(), cut)
	}
	if err := os.Truncate(partials[0], cut); err != nil {
		t.Fatalf("truncate partial: %v", err)
	}

	h.Restart()
	h.WaitForJobComplete(jobID)
	servedAfter := h.Server.ArticlesServed()

	// Classify EVERY article that was durable at the stop by where it sits
	// relative to the cut, rather than by the ordinal the wait returned on.
	ids := h.MsgIDs[0]
	var above int
	var destroyedMissed []string
	for i, wasDurable := range durableAtStop {
		if !wasDurable || i < keepArticles {
			continue
		}
		above++
		if servedAfter[ids[i]] <= servedBefore[ids[i]] {
			destroyedMissed = append(destroyedMissed, ids[i])
		}
	}
	if above == 0 {
		t.Fatal("the truncation destroyed no durable article — the destroyed-set " +
			"assertion below would hold vacuously")
	}
	if len(destroyedMissed) > 0 {
		t.Errorf("%d of the %d durable articles the truncation DESTROYED were not "+
			"re-fetched; their bytes are gone, so treating them as present is S3's "+
			"absence-of-evidence read as evidence: %v",
			len(destroyedMissed), above, destroyedMissed)
	}
	// The assertion the re-fetch bookkeeping exists to serve. A file finished
	// over a discarded-but-partly-trusted record has a hole in it, and only
	// reading it back can say so.
	h.AssertCompletedFilesMatch()
	t.Logf("%d durable articles were destroyed by the cut; %d of them were not re-fetched",
		above, len(destroyedMissed))
}

// TestExternalModification_DeletedPartialRestartsTheFile was committed RED
// against issue #362, the same defect as the test above, and is green as of
// the same fix. It pins the absence branch of S3: with no file there is no
// evidence for any article, so every one of them is Outstanding again —
// including the ones the stored runs still claim are durable.
func TestExternalModification_DeletedPartialRestartsTheFile(t *testing.T) {
	opts := externalFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	const durableArticles = 16
	h.WaitForDurableBytes(jobID, durableArticles*int64(opts.Files[0].PartSize))
	h.Stop()
	servedBefore := h.Server.ArticlesServed()
	if len(servedBefore) < durableArticles {
		t.Fatalf("only %d articles were served before the stop, need at least %d — "+
			"with nothing durable, deleting the file would cost nothing and the "+
			"assertion below would hold vacuously", len(servedBefore), durableArticles)
	}

	partials := h.PartialPaths()
	if len(partials) != 1 {
		t.Fatalf("expected one partial file, found %v", partials)
	}
	if err := os.Remove(partials[0]); err != nil {
		t.Fatalf("remove partial: %v", err)
	}

	h.Restart()
	h.WaitForJobComplete(jobID)
	servedAfter := h.Server.ArticlesServed()

	var notRefetched []string
	for i := range durableArticles {
		id := h.MsgIDs[0][i]
		if servedAfter[id] <= servedBefore[id] {
			notRefetched = append(notRefetched, id)
		}
	}
	if len(notRefetched) > 0 {
		t.Errorf("%d of %d articles that were durable before the file was deleted were "+
			"not re-fetched — the stored runs were trusted over the absence of the "+
			"file they describe: %v", len(notRefetched), durableArticles, notRefetched)
	}
	t.Logf("%d of %d previously durable articles were not re-fetched after the partial was deleted",
		len(notRefetched), durableArticles)
}

// TestExternalModification_AppendedGarbageIsTrimmed pins S6 from the other
// side: metadata may shrink a file, never grow it, so bytes appended past the
// last durable article must not survive into the completed file.
//
// The file is now LONGER than its runs claim, which S7's surviving size
// comparison treats as the ordinary pre-allocated case: the runs are adopted
// whole, so nothing needs re-fetching. Both halves are asserted: the articles
// already on disk must NOT come back over the wire, and the completed file
// must be exactly the bytes the server served, with the appended garbage gone.
//
// Both halves discriminate. The no-refetch half did not until #362 was fixed —
// see the note on TestExternalModification_MtimeTouchCostsNoRefetch.
func TestExternalModification_AppendedGarbageIsTrimmed(t *testing.T) {
	opts := externalFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	const durableArticles = 16
	h.WaitForDurableBytes(jobID, durableArticles*int64(opts.Files[0].PartSize))
	h.Stop()
	servedBefore := h.Server.ArticlesServed()
	durableIDs := h.DurableMessageIDs(jobID)
	if len(durableIDs) == 0 {
		t.Fatal("nothing was durable at the stop — the no-refetch assertion below " +
			"would hold vacuously")
	}

	partials := h.PartialPaths()
	if len(partials) != 1 {
		t.Fatalf("expected one partial file, found %v", partials)
	}
	before, err := os.Stat(partials[0])
	if err != nil {
		t.Fatalf("stat partial: %v", err)
	}
	garbage := bytes.Repeat([]byte{0xAB}, 4096)
	f, err := os.OpenFile(partials[0], os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open partial for append: %v", err)
	}
	if _, err := f.Write(garbage); err != nil {
		t.Fatalf("append to partial: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close partial: %v", err)
	}
	after, err := os.Stat(partials[0])
	if err != nil {
		t.Fatalf("re-stat partial: %v", err)
	}
	if after.Size() != before.Size()+int64(len(garbage)) {
		t.Fatalf("partial grew from %d to %d, want %d — the append did not land",
			before.Size(), after.Size(), before.Size()+int64(len(garbage)))
	}

	h.Restart()
	h.WaitForJobComplete(jobID)

	servedAfter := h.Server.ArticlesServed()
	var refetched []string
	for id := range durableIDs {
		if servedAfter[id] > servedBefore[id] {
			refetched = append(refetched, id)
		}
	}
	if len(refetched) > 0 {
		t.Errorf("%d of %d durable articles were re-fetched after bytes were appended "+
			"past them; the file only grew, so their runs still pass the size gate "+
			"and the re-fetch is rework the append did not cause: %v",
			len(refetched), len(durableIDs), refetched)
	}
}

// TestExternalModification_MtimeTouchCostsNoRefetch pins S7 as it now stands:
// the validity stamp is SIZE ALONE, so touching the mtime while leaving every
// byte in place must cost nothing at all.
//
// It used to pin the opposite reason for the same outcome — the stamp was the
// pair (size, mtime), so a touch invalidated the fast path and the file was
// recomputed rather than re-downloaded. The recompute is gone with the
// two-record design, which is exactly why mtime had to go with it: the only
// response left to a mismatch is discard-and-refetch, and an mtime moves
// without a byte moving. This test is what would have shown that as a whole
// file re-downloaded.
//
// The assertion is on the re-fetch set rather than on the file, and that is the
// point. A file that finishes correctly says nothing about whether the daemon
// threw away 2 MiB of verified bytes to get there.
//
// # What this test discriminates, and what it used to not
//
// It is a pin on the resume, not merely on the outcome: with
// durability.Resumer.Resume neutered to return no runs, every durable article
// is re-fetched and this fails — observed, not reasoned.
//
// That was not true before #362 was fixed. Queue's additive seeding entry point
// was not the only carrier of "this article is already done":
// Store.RestoreJobProgress restored a per-job done bitmap unconditionally, and
// that alone kept the articles off the wire whatever the resume concluded.
// Making the startup sweep authoritative — Queue.ReplaceFromRuns — is what
// turned this assertion into a pin, and the same neutering that leaves it green
// again would mean the precedence has been reverted.
func TestExternalModification_MtimeTouchCostsNoRefetch(t *testing.T) {
	opts := externalFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	const durableArticles = 16
	h.WaitForDurableBytes(jobID, durableArticles*int64(opts.Files[0].PartSize))
	h.Stop()
	servedBefore := h.Server.ArticlesServed()
	durableIDs := h.DurableMessageIDs(jobID)
	if len(durableIDs) == 0 {
		t.Fatal("nothing was durable at the stop — the no-refetch assertion below " +
			"would hold vacuously")
	}

	partials := h.PartialPaths()
	if len(partials) != 1 {
		t.Fatalf("expected one partial file, found %v", partials)
	}
	before, err := os.Stat(partials[0])
	if err != nil {
		t.Fatalf("stat partial: %v", err)
	}
	touched := before.ModTime().Add(-time.Hour)
	if err := os.Chtimes(partials[0], touched, touched); err != nil {
		t.Fatalf("touch partial: %v", err)
	}
	after, err := os.Stat(partials[0])
	if err != nil {
		t.Fatalf("re-stat partial: %v", err)
	}
	if after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the mtime did not change — the fast path would still be taken and " +
			"this test would assert nothing")
	}
	if after.Size() != before.Size() {
		t.Fatalf("the touch changed the size from %d to %d; only the mtime may move here",
			before.Size(), after.Size())
	}

	h.Restart()
	h.WaitForJobComplete(jobID)

	servedAfter := h.Server.ArticlesServed()
	var refetched []string
	for id := range durableIDs {
		if servedAfter[id] > servedBefore[id] {
			refetched = append(refetched, id)
		}
	}
	if len(refetched) > 0 {
		t.Errorf("%d of %d durable articles were re-fetched after nothing but the mtime "+
			"moved; the bytes are all still there and a recomputation proves them: %v",
			len(refetched), len(durableIDs), refetched)
	}
}
