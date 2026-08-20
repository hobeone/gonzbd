package durability

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// The tests in this file close gaps that gremlins found and the existing suite
// could not see. Each one names the mutant it kills, because in every case an
// adjacent test already asserted the right property with a fixture that could
// not distinguish the code from the mutation — the same inert-fixture shape
// this package's history is full of, arrived at mechanically rather than by
// review.

// TestBitmap_IndexingAcrossAWordBoundary pins Bitmap at more than one word.
//
// Every other bitmap test in this package uses a handful of bits, so the whole
// package only ever exercised a single 64-bit word. Two mutants survive on
// that: (n+63)/64 becoming (n-63)/64 in NewBitmap, and i >= b.n becoming
// i > b.n in Get. Both are invisible below 64 bits and both panic above it.
//
// 128 is chosen rather than 130 because Get's boundary only misbehaves when
// b.n is an exact multiple of 64: there Get(b.n) indexes word b.n/64, which is
// one past the last allocated word. At 130 the mutant reads a word that
// happens to exist and returns a plausible false.
func TestBitmap_IndexingAcrossAWordBoundary(t *testing.T) {
	const n = 128
	b := NewBitmap(n)
	if b.Len() != n {
		t.Fatalf("Len = %d, want %d", b.Len(), n)
	}

	// Set the first bit of each word and the very last bit. Under the
	// (n-63)/64 mutant only one word is allocated, so Set(64) panics.
	for _, i := range []int{0, 64, 127} {
		b.Set(i)
	}
	for _, i := range []int{0, 64, 127} {
		if !b.Get(i) {
			t.Errorf("Get(%d) = false after Set(%d); the bitmap is not addressing "+
				"the word this bit lives in", i, i)
		}
	}
	if b.Get(1) || b.Get(63) || b.Get(126) {
		t.Error("a bit no Set touched reads as set")
	}
	if b.Count() != 3 {
		t.Errorf("Count = %d, want 3", b.Count())
	}

	// Get at exactly Len must report false rather than index past the last
	// word. Under the i > b.n mutant this indexes words[2] of a 2-word bitmap
	// and panics.
	if b.Get(n) {
		t.Errorf("Get(%d) = true for an index at Len; out-of-range must read false", n)
	}
	if b.Get(-1) {
		t.Error("Get(-1) = true; a negative index must read false")
	}
}

// TestResume_FastPathWidensAStoredBitmapNarrowerByAWholeWord pins the widening
// arithmetic in Resumer.committedExtent.
//
// TestResume_FastPathWidensANarrowerStoredBitmap already covers "narrower",
// but with an article count under 64 — where a one-word buffer is ALREADY the
// full width, so `len(raw) < need` is false and the widening branch never
// runs at all. This test is the only one that executes it.
//
// It kills `(artCount+63)` becoming `(artCount-63)`, which at counts under 64
// leaves the buffer alone either way. It does NOT kill the two mutants that
// still survive at this line, and neither can be killed: `/ 64` becoming
// `* 64` over-allocates and then BitmapFromBytes reads only the first `need`
// words, and `<` becoming `<=` copies an already-correct buffer into a new
// slice of the same length. Both produce a byte-identical bitmap.
func TestResume_FastPathWidensAStoredBitmapNarrowerByAWholeWord(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x01}, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Stored bitmap is one word wide; the file now has 130 articles, which
	// needs three. Bit 3 is set so the adopted bitmap can be checked for
	// content, not merely for width.
	stored := NewBitmap(64)
	stored.Set(3)
	exts := NewSQLiteExtentStore(openTestDB(t))
	if err := exts.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: stored, Size: fi.Size(), ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), exts, testLogger(t))
	res, err := r.Resume(ctx, "job-1", 0, path, 0, 130)
	if err != nil {
		t.Fatalf("Resume over a stored bitmap narrower than the article count: %v", err)
	}
	if res.Durable.Len() != 130 {
		t.Fatalf("adopted bitmap is %d bits wide, want 130", res.Durable.Len())
	}
	if !res.Durable.Get(3) {
		t.Error("bit 3 was set in the stored bitmap and is clear after widening")
	}
	if res.Durable.Count() != 1 {
		t.Errorf("Count = %d, want 1 — widening must zero-fill, not invent bits", res.Durable.Count())
	}
}

// TestBarrier_PriorExtentWidensAStoredBitmapNarrowerByAWholeWord is the same
// pin for Barrier.priorExtent, which carries its own copy of the widening
// arithmetic. The same two equivalent mutants survive there for the same
// reasons — see the test above.
func TestBarrier_PriorExtentWidensAStoredBitmapNarrowerByAWholeWord(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	exts := NewSQLiteExtentStore(db)

	stored := NewBitmap(64)
	stored.Set(5)
	if err := exts.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: stored}}); err != nil {
		t.Fatal(err)
	}

	b := NewBarrier(NewSQLiteFactLog(db), exts, &recordingAcker{}, &recordingStall{},
		slog.New(slog.DiscardHandler))
	ext, err := b.priorExtent(ctx, "job-1", 0, 130)
	if err != nil {
		t.Fatalf("priorExtent over a stored bitmap narrower than the article count: %v", err)
	}
	if ext.Durable.Len() != 130 {
		t.Fatalf("re-derived bitmap is %d bits wide, want 130", ext.Durable.Len())
	}
	if !ext.Durable.Get(5) || ext.Durable.Count() != 1 {
		t.Errorf("re-derived bitmap lost the stored bit: Get(5)=%v Count=%d",
			ext.Durable.Get(5), ext.Durable.Count())
	}
}

// TestResume_FactAtExactlyTheArticleCountFailsLoudly closes the boundary that
// TestResume_FactBeyondTheArticleCountFailsLoudly leaves open.
//
// That test uses ArtIdx 7 against an article count of 2. Both `ord >= artCount`
// and the mutant `ord > artCount` reject 7, so it passes either way. The only
// index that separates them is artCount itself — the first invalid one.
//
// The consequence of the mutant is not a panic but silence: recompute would
// accept the index, call durable.Set(ord), and Bitmap.Set would drop it as
// out of range. The article ends up neither proven nor reported, which is
// exactly the "silently clear bit" the Resume doc forbids under A2/R28.
func TestResume_FactAtExactlyTheArticleCountFailsLoudly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	if err := os.WriteFile(path, a0, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	// firstArtIdx is 0 and artCount is 2, so ArtIdx 2 has ord == artCount.
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 2, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	_, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err == nil {
		t.Fatal("Resume accepted a fact whose ordinal equals the article count; " +
			"the bit it sets is dropped by Bitmap.Set, so the article is silently " +
			"neither proven nor reported")
	}
	if !errors.Is(err, ErrArticleOutOfRange) {
		t.Errorf("err = %v, want one wrapping ErrArticleOutOfRange", err)
	}
}

// TestResume_VerifiesAFactEndingExactlyAtEndOfFile pins the containment test
// in recompute against its own boundary.
//
// `fact.Offset > size-int64(fact.Length)` becoming `>=` rejects a region that
// ends exactly at EOF. That is not an exotic input: it is the LAST ARTICLE OF
// EVERY COMPLETE FILE, and the mutant would leave it permanently Outstanding.
//
// Unlike its neighbours in this file, this boundary was ALREADY pinned — the
// mutation never appeared in a gremlins LIVED list, and an existing test kills
// it. It is kept because the property is worth stating where a reader will
// find it, but it closed no gap. The mutant that does survive at this line is
// `fact.Length < 0` becoming `<=`, which only a zero-length fact could tell
// apart; no article has one, so nothing here chases it.
func TestResume_VerifiesAFactEndingExactlyAtEndOfFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100)
	if err := os.WriteFile(path, append(append([]byte{}, a0...), a1...), 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
	}); err != nil {
		t.Fatal(err)
	}

	// No committed extent, so Resume falls through to recompute.
	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	res, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !res.Durable.Get(0) {
		t.Error("article 0 is not durable; it sits wholly inside the file and its CRC matches")
	}
	if !res.Durable.Get(1) {
		t.Fatal("the article ending exactly at EOF was left Outstanding. " +
			"Offset(100) > size(200)-Length(100) is false, so its region IS wholly " +
			"inside the file — rejecting it re-fetches the last article of every " +
			"complete file, forever")
	}
	if res.VerifiedTo != 200 {
		t.Errorf("VerifiedTo = %d, want 200 — the gapless prefix must reach EOF", res.VerifiedTo)
	}
}

// TestFinalizeFile_FactsButNoneDurableDoesNotTruncate closes the fixture gap in
// TestFinalizeFile_NoDurableFactsDoesNotTruncate.
//
// That test's fixture has no facts AT ALL, so the recorded extent is also 0 and
// `bound > 0` becoming `bound >= 0` changes nothing. Facts that exist but name
// no durable article separate the two: under the mutant FinalizeFile enters the
// fallback block, finds missing > 0, adopts the recorded bound and truncates a
// file it was never entitled to touch.
func TestFinalizeFile_FactsButNoneDurableDoesNotTruncate(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)

	// Two facts, and nothing durable: no prior extent, and the drain is empty.
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100},
	}); err != nil {
		t.Fatal(err)
	}

	tgt := &factGapTarget{artCount: 2, size: 5000}
	b := NewBarrier(facts, NewSQLiteExtentStore(db), &recordingAcker{}, &recordingStall{},
		slog.New(slog.DiscardHandler))
	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}
	if tgt.called {
		t.Fatalf("truncated to %d with no durable article. The bound must come from "+
			"the DURABLE set; recorded facts alone say only that articles decoded, "+
			"never that their bytes reached disk", tgt.bound)
	}
}
