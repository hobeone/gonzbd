//go:build crash && linux

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
		Connections: 1,
		BodyDelay:   4 * time.Millisecond,
		Files:       []fileSpec{{Name: "payload.bin", Size: 8 << 20, PartSize: 128 << 10}},
	}
}

// TestExternalModification_TruncatedPartialIsRecomputed: a clean stop records
// every article written, and the file is then cut in half at an article
// boundary. The restart reads each recorded article back, so:
//
//   - every recorded article BELOW the cut verifies and is not fetched again;
//   - every recorded article the cut destroyed is fetched again — treating a
//     destroyed article as present is what finished a file with a hole in it
//     (#362);
//   - the completed file matches its expected content byte for byte.
func TestExternalModification_TruncatedPartialIsRecomputed(t *testing.T) {
	t.Parallel()
	opts := externalFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	// 32 articles written = 4 MiB from byte 0.
	const writtenArticles = 32
	h.WaitForWrittenBytes(jobID, writtenArticles*int64(opts.Files[0].PartSize))
	h.Stop()
	servedBefore := h.Server.ArticlesServed()

	// The recorded set at the stop. Read rather than assumed to be the first
	// `writtenArticles` articles: a clean stop flushes everything written, so
	// the real set reaches past the threshold the wait above returned on.
	recordedAtStop := h.RecordedOrdinals(jobID)[0]
	if len(recordedAtStop) == 0 {
		t.Fatal("no article was recorded for file 0 at the stop")
	}

	// Cut the file in half, at an article boundary, so the surviving and
	// destroyed sets are exact rather than approximate.
	const keepArticles = writtenArticles / 2
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

	// Classify EVERY article recorded at the stop by where it sits relative
	// to the cut, rather than by the ordinal the wait returned on.
	ids := h.MsgIDs[0]
	var above, below int
	var destroyedMissed, survivorsRefetched []string
	for i, wasRecorded := range recordedAtStop {
		if !wasRecorded {
			continue
		}
		if i < keepArticles {
			below++
			if servedAfter[ids[i]] > servedBefore[ids[i]] {
				survivorsRefetched = append(survivorsRefetched, ids[i])
			}
			continue
		}
		above++
		if servedAfter[ids[i]] <= servedBefore[ids[i]] {
			destroyedMissed = append(destroyedMissed, ids[i])
		}
	}
	if above == 0 || below == 0 {
		t.Fatalf("the cut left %d recorded articles below it and destroyed %d — one "+
			"of the assertions below would hold vacuously", below, above)
	}
	if len(destroyedMissed) > 0 {
		t.Errorf("%d of the %d recorded articles the truncation DESTROYED were not "+
			"re-fetched; their bytes are gone: %v", len(destroyedMissed), above, destroyedMissed)
	}
	if len(survivorsRefetched) > 0 {
		t.Errorf("%d of the %d recorded articles below the cut were re-fetched; their "+
			"bytes were intact and read back: %v", len(survivorsRefetched), below, survivorsRefetched)
	}
	// A file finished over a partly-trusted record has a hole in it, and only
	// reading it back can say so.
	h.AssertCompletedFilesMatch()
}

// TestExternalModification_DeletedPartialRestartsTheFile: with no file there
// is no evidence for any article, so every one of them is Outstanding again —
// including the ones written_articles still records.
func TestExternalModification_DeletedPartialRestartsTheFile(t *testing.T) {
	t.Parallel()
	opts := externalFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	const writtenArticles = 16
	h.WaitForWrittenBytes(jobID, writtenArticles*int64(opts.Files[0].PartSize))
	h.Stop()
	servedBefore := h.Server.ArticlesServed()
	if len(servedBefore) < writtenArticles {
		t.Fatalf("only %d articles were served before the stop, need at least %d — "+
			"with nothing written, deleting the file would cost nothing and the "+
			"assertion below would hold vacuously", len(servedBefore), writtenArticles)
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
	for i := range writtenArticles {
		id := h.MsgIDs[0][i]
		if servedAfter[id] <= servedBefore[id] {
			notRefetched = append(notRefetched, id)
		}
	}
	if len(notRefetched) > 0 {
		t.Errorf("%d of %d articles written before the file was deleted were not "+
			"re-fetched — the stored rows were trusted over the absence of the file "+
			"they describe: %v", len(notRefetched), writtenArticles, notRefetched)
	}
}

// TestExternalModification_AppendedGarbageIsTrimmed: bytes appended past the
// last written article must not survive into the completed file, and the
// articles already on disk, which still read back, must not come back over the
// wire.
func TestExternalModification_AppendedGarbageIsTrimmed(t *testing.T) {
	t.Parallel()
	opts := externalFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	const writtenArticles = 16
	h.WaitForWrittenBytes(jobID, writtenArticles*int64(opts.Files[0].PartSize))
	h.Stop()
	servedBefore := h.Server.ArticlesServed()
	recordedIDs := h.RecordedMessageIDs(jobID)
	if len(recordedIDs) == 0 {
		t.Fatal("nothing was recorded at the stop — the no-refetch assertion below " +
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
	for id := range recordedIDs {
		if servedAfter[id] > servedBefore[id] {
			refetched = append(refetched, id)
		}
	}
	if len(refetched) > 0 {
		t.Errorf("%d of %d recorded articles were re-fetched after bytes were appended "+
			"past them; their bytes still read back, so the re-fetch is rework the "+
			"append did not cause: %v", len(refetched), len(recordedIDs), refetched)
	}
}

// TestExternalModification_MtimeTouchCostsNoRefetch: touching the mtime while
// leaving every byte in place costs nothing at all. The assertion is on the
// re-fetch set rather than on the file: a file that finishes correctly says
// nothing about whether the daemon threw away verified bytes to get there.
func TestExternalModification_MtimeTouchCostsNoRefetch(t *testing.T) {
	t.Parallel()
	opts := externalFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	const writtenArticles = 16
	h.WaitForWrittenBytes(jobID, writtenArticles*int64(opts.Files[0].PartSize))
	h.Stop()
	servedBefore := h.Server.ArticlesServed()
	recordedIDs := h.RecordedMessageIDs(jobID)
	if len(recordedIDs) == 0 {
		t.Fatal("nothing was recorded at the stop — the no-refetch assertion below " +
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
		t.Fatal("the mtime did not change — this test would assert nothing")
	}
	if after.Size() != before.Size() {
		t.Fatalf("the touch changed the size from %d to %d; only the mtime may move here",
			before.Size(), after.Size())
	}

	h.Restart()
	h.WaitForJobComplete(jobID)

	servedAfter := h.Server.ArticlesServed()
	var refetched []string
	for id := range recordedIDs {
		if servedAfter[id] > servedBefore[id] {
			refetched = append(refetched, id)
		}
	}
	if len(refetched) > 0 {
		t.Errorf("%d of %d recorded articles were re-fetched after nothing but the mtime "+
			"moved; the bytes are all still there and read back: %v",
			len(refetched), len(recordedIDs), refetched)
	}
}
