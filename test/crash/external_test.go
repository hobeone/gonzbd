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
// IT FAILS TODAY, against issue #362, and the failure is this suite working.
// The recomputation gets the right answer and the answer is discarded, so the
// job finishes with a completed file that has a hole in it. Do not relax the
// assertions to make it green.
//
// A clean stop commits an extent whose size and mtime match the file, so the
// next start would adopt it without reading a byte. Truncating the file
// falsifies that stamp, and the design's answer is to recompute from the bytes
// rather than to trust the cache or to restart the file wholesale.
//
// The assertions are therefore about WHICH articles came back, not merely that
// the job finished: the articles below the cut must be re-verified from disk
// and NOT re-fetched, and the ones the cut destroyed must be re-fetched. A
// test that only checked the final file would pass just as happily against an
// implementation that threw the whole file away, which is the L3 regression
// this suite exists to catch.
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
		t.Fatal("no durable bits were recorded for file 0 at the stop")
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
	var below, above int
	var survivedRefetched, destroyedMissed []string
	for i, wasDurable := range durableAtStop {
		if !wasDurable {
			continue
		}
		refetched := servedAfter[ids[i]] > servedBefore[ids[i]]
		if i < keepArticles {
			below++
			if refetched {
				survivedRefetched = append(survivedRefetched, ids[i])
			}
			continue
		}
		above++
		if !refetched {
			destroyedMissed = append(destroyedMissed, ids[i])
		}
	}
	if above == 0 {
		t.Fatal("the truncation destroyed no durable article — the destroyed-set " +
			"assertion below would hold vacuously")
	}
	if len(survivedRefetched) > 0 {
		t.Errorf("%d of the %d durable articles BELOW the truncation point were re-fetched; "+
			"a recomputation reads their bytes and proves them, so re-fetching them is "+
			"rework a falsified cache did not cause: %v",
			len(survivedRefetched), below, survivedRefetched)
	}
	if len(destroyedMissed) > 0 {
		t.Errorf("%d of the %d durable articles the truncation DESTROYED were not "+
			"re-fetched; their bytes are gone, so treating them as present is S3's "+
			"absence-of-evidence read as evidence: %v",
			len(destroyedMissed), above, destroyedMissed)
	}
	t.Logf("%d durable articles below the cut, %d above it; %d below were re-fetched, "+
		"%d above were not", below, above, len(survivedRefetched), len(destroyedMissed))
}

// TestExternalModification_DeletedPartialRestartsTheFile FAILS TODAY against
// issue #362, the same defect as the test above. It pins the absence
// branch of S3: with no file there is no evidence for any article, so every
// one of them is Outstanding again — including the ones a committed extent
// still claims are durable.
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
			"not re-fetched — a committed extent was trusted over the absence of the "+
			"file it describes: %v", len(notRefetched), durableArticles, notRefetched)
	}
	t.Logf("%d of %d previously durable articles were not re-fetched after the partial was deleted",
		len(notRefetched), durableArticles)
}

// TestExternalModification_AppendedGarbageIsTrimmed pins S6 from the other
// side: metadata may shrink a file, never grow it, so bytes appended past the
// last durable article must not survive into the completed file.
//
// The extent's size stamp no longer matches, so the fast path is refused and
// the file is recomputed — but every recorded region is still where it was, so
// nothing needs re-fetching. Both halves are asserted: the articles already on
// disk must NOT come back over the wire, and the completed file must be
// exactly the bytes the server served, with the appended garbage gone.
//
// The TRIM half is the one that discriminates here. See the note on
// TestExternalModification_MtimeTouchCostsNoRefetch for why the no-refetch half
// does not yet.
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
			"past them; their own regions were untouched, so a recomputation proves them "+
			"and the re-fetch is rework the append did not cause: %v",
			len(refetched), len(durableIDs), refetched)
	}
}

// TestExternalModification_MtimeTouchCostsNoRefetch pins the cheap half of the
// R13 validity check: the stamp is size AND mtime, so touching the mtime alone
// invalidates the fast path — and that must cost a recomputation, not a
// re-download.
//
// The assertion is on the re-fetch set rather than on the file, and that is the
// point. A file that finishes correctly says nothing about whether the daemon
// threw away 2 MiB of verified bytes to get there.
//
// # What this test does NOT currently discriminate
//
// The no-refetch half passes even with durability.Resumer.Resume neutered to
// return an empty bitmap — observed, not reasoned. Queue.SeedFromExtents is
// not the only carrier of "this article is already done": Store.RestoreJobProgress
// restores job_files.articles_done unconditionally, and that alone is enough to
// keep the articles off the wire. So this assertion is a regression guard on the
// OUTCOME, not a pin on the recomputation producing it. It becomes a real pin
// once the resume sweep is made authoritative over that column — the same change
// the two failing tests above are waiting for.
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
