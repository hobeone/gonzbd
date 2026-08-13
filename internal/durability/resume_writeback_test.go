package durability

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResume_WritesTheRecomputationBackToTheStore pins S4 against the store
// rather than only against the queue.
//
// "Where Class B disagrees with a recomputation, the recomputation is correct
// by definition" was applied to the live job and never written back. Nothing
// clears a bit in file_extents: Durable.Set is the only bit mutation in the
// package, and the sweep committed no extent at all. So a bit the
// recomputation disproved stayed in the store, and three separate paths
// resurrected it:
//
//   - Barrier.priorExtent adopts the stored bitmap as an OR-base, so the next
//     checkpoint re-commits the disproven bit with a FRESH Size/ModTimeNs from
//     its own Stat — a stamp that then validates against the file on disk.
//   - Resumer.Resume's stat fast path adopts that bitmap on the next start
//     without reading a byte, and ReplaceFromResume marks the article done.
//   - reevaluateStall's third phase loads the same rows and feeds them to the
//     additive SeedFromExtents, re-setting the bits in-process.
//
// The fixture disproves article 1 by corrupting its bytes, so the
// recomputation must clear it. The assertion is on the STORE, because the
// in-memory answer was already correct and is not what resurrects.
func TestResume_WritesTheRecomputationBackToTheStore(t *testing.T) {
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

	// A previous run recorded both articles durable, with a stamp that no
	// longer matches (an earlier size), so Resume must recompute.
	exts := NewSQLiteExtentStore(openTestDB(t))
	prior := NewBitmap(2)
	prior.Set(0)
	prior.Set(1)
	if err := exts.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: prior, BytesDurable: 200, Size: 999, ModTimeNs: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	// Article 1's bytes on disk no longer match its recorded CRC.
	fh, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt(bytes.Repeat([]byte{0xFF}, 100), 100); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, exts, testLogger(t))
	res, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Grounding: the fixture must actually have recomputed and disproved
	// article 1, or the store assertion below proves nothing.
	if !res.Recomputed {
		t.Fatal("fixture was adopted from the cache, so it never disproved anything")
	}
	if res.Durable.Get(1) {
		t.Fatal("fixture did not disprove article 1 in memory")
	}
	if !res.Durable.Get(0) {
		t.Fatal("fixture wrongly disproved article 0, whose bytes are intact")
	}

	stored, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("Load returned %d extents, want 1", len(stored))
	}
	got := stored[0]
	if got.Durable.Get(1) {
		t.Fatal("the store still records article 1 as durable after the recomputation " +
			"disproved it. priorExtent ORs this bitmap as its base, so the next " +
			"checkpoint re-commits the bit with a fresh stamp that validates, and the " +
			"next start adopts it without reading a byte")
	}
	if !got.Durable.Get(0) {
		t.Error("the store lost article 0, which the recomputation confirmed")
	}
	// The written-back record must describe the file it was derived from, or
	// the next resume's S7 check throws away a cache that is actually valid.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != fi.Size() || got.ModTimeNs != fi.ModTime().UnixNano() {
		t.Errorf("stored stamp = (%d, %d), want (%d, %d) — a stamp that does not "+
			"match the file fails S7 on the next start and discards a valid cache",
			got.Size, got.ModTimeNs, fi.Size(), fi.ModTime().UnixNano())
	}
	if got.BytesDurable != 100 {
		t.Errorf("BytesDurable = %d, want 100 — committing without it zeroes the "+
			"figure the API reports for this file", got.BytesDurable)
	}
}

// failingExtentStore fails Commit while behaving normally otherwise, so a
// write-back failure can be observed without breaking the load the resume
// performs first.
type failingExtentStore struct {
	ExtentStore
	err error
}

func (f failingExtentStore) Commit(context.Context, string, []FileExtent) error { return f.err }

// TestResumeWriteBack_SurfacesACommitFailure pins that a failed write-back is
// returned rather than logged.
//
// It is the correction's own durability that failed. Swallowing it would let
// the sweep report a clean resume while the store still records the disproven
// article, which is precisely the state the write-back exists to leave behind
// — and the caller would have no way to know the difference.
func TestResumeWriteBack_SurfacesACommitFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	if err := os.WriteFile(path, a0, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
	}); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("database is locked")
	exts := failingExtentStore{ExtentStore: NewSQLiteExtentStore(openTestDB(t)), err: boom}
	r := NewResumer(fl, exts, testLogger(t))

	// No committed extent, so this recomputes and therefore writes back.
	_, err := r.Resume(ctx, "job-1", 0, path, 0, 1)
	if err == nil {
		t.Fatal("Resume reported success while its write-back failed; the store still " +
			"holds the record the recomputation disproved and nothing says so")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want one wrapping the commit failure", err)
	}
	if !strings.Contains(err.Error(), "write-back") {
		t.Errorf("err = %q, want it to name the write-back so the failing step is "+
			"identifiable from the message alone", err)
	}
}

// TestResumeWriteBack_CarriesEveryField pins that the committed record is built
// wholly from the recomputation.
//
// A partially-filled extent is worse than none: BytesDurable omitted zeroes the
// figure the API reports for the file, and a Size/ModTimeNs that does not match
// the file fails S7 on the next start and discards a cache that is valid.
func TestResumeWriteBack_CarriesEveryField(t *testing.T) {
	ctx := context.Background()
	exts := NewSQLiteExtentStore(openTestDB(t))
	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), exts, testLogger(t))

	bm := NewBitmap(3)
	bm.Set(2)
	res := ResumeResult{
		Durable: bm, VerifiedTo: 300, PrefixCRC: 0xABCD, HasPrefixCRC: true,
		BytesDurable: 100, Size: 300, ModTimeNs: 424242,
	}
	if err := r.writeBack(ctx, "job-1", 7, res); err != nil {
		t.Fatalf("writeBack: %v", err)
	}

	stored, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("Load returned %d extents, want 1", len(stored))
	}
	got := stored[0]
	if got.FileIdx != 7 || got.VerifiedTo != 300 || got.PrefixCRC != 0xABCD ||
		!got.HasPrefixCRC || got.BytesDurable != 100 || got.Size != 300 || got.ModTimeNs != 424242 {
		t.Errorf("written-back extent lost a field: %+v", got)
	}
	if !got.Durable.Get(2) || got.Durable.Count() != 1 {
		t.Errorf("written-back bitmap = %d bits set, want exactly bit 2", got.Durable.Count())
	}
}
