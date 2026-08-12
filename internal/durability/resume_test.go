package durability

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/crc32util"
)

// TestResume_FastPathAdoptsWhenStampMatches pins the O(1) path (B3).
func TestResume_FastPathAdoptsWhenStampMatches(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(4)
	bm.Set(0)
	bm.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 200, PrefixCRC: 0x1234, HasPrefixCRC: true,
		Size: 300, ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recomputed {
		t.Error("Recomputed = true with a matching stamp — the fast path did not fire")
	}
	if got.Restart {
		t.Error("Restart = true for a file whose stamp matched")
	}
	if got.Durable.Count() != 2 || got.VerifiedTo != 200 || got.PrefixCRC != 0x1234 {
		t.Errorf("fast path did not adopt the cache: %+v", got)
	}
	// The bitmap must come back at the article count, not at the byte width
	// persistence rounded it up to. ExtentStore.Load cannot apply Bitmap's
	// tail-word mask — its n is always a multiple of 64 — so a bitmap adopted
	// as loaded carries any padding bits a damaged blob set, and Count() then
	// over-reports how many articles are durable.
	if got.Durable.Len() != 4 {
		t.Errorf("Durable.Len() = %d, want 4 — the stored bitmap was adopted at its byte width", got.Durable.Len())
	}
	// The committed extent carries HasPrefixCRC: true over a VerifiedTo of
	// 200 in a 300-byte file, which is not a whole-file CRC. The stamp
	// matching says the file has not changed; it says nothing about whether
	// the flag was still true when it was written. See Resume's comment on
	// how a flag outlives its condition when a file grows past a hole.
	if got.HasPrefixCRC {
		t.Error("HasPrefixCRC = true although the cached prefix covers 200 of the file's 300 bytes")
	}
}

// TestResume_TruncatedFileForcesRecompute pins S7 against the external
// modification failure mode: the cache was correct when written and the
// world changed underneath it.
func TestResume_TruncatedFileForcesRecompute(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")

	// Two articles of 100 bytes each, with known CRCs.
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

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(2)
	bm.Set(0)
	bm.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 200, Size: 200, ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	// The user truncates the partial file between runs.
	if err := os.Truncate(path, 100); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Recomputed {
		t.Fatal("Recomputed = false after the file was truncated — the cache was adopted unvalidated")
	}
	if got.Durable.Get(1) {
		t.Error("article 1 is still durable after its bytes were truncated away")
	}
	if !got.Durable.Get(0) {
		t.Error("article 0 was discarded although its bytes are intact and match their CRC")
	}
	if got.VerifiedTo != 100 {
		t.Errorf("VerifiedTo = %d, want 100", got.VerifiedTo)
	}
	// The prefix covers every byte the truncated file still holds, but a
	// recorded fact lies wholly beyond its end, so what is on disk is not the
	// whole of what this file is meant to contain. Reporting a whole-file CRC
	// here would label a chopped file's CRC as the file's (R23).
	if got.HasPrefixCRC {
		t.Error("HasPrefixCRC = true although a recorded fact lies beyond the truncated file's end")
	}
}

// TestResume_SameSizeEditForcesRecompute pins the mtime half of the S7
// validity stamp. An edit in place — a partial file opened and rewritten by
// another tool — leaves the size identical and only the mtime moves, so a
// guard that checks size alone adopts a cache describing bytes that are gone.
func TestResume_SameSizeEditForcesRecompute(t *testing.T) {
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
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(2)
	bm.Set(0)
	bm.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 200, Size: 200, ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	// Same length, different bytes in the second half, and a moved mtime.
	edited := append(append([]byte{}, a0...), bytes.Repeat([]byte{0x03}, 100)...)
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	moved := fi.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, moved, moved); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Recomputed {
		t.Fatal("Recomputed = false after a same-size edit — only the mtime moved and the cache was adopted")
	}
	if got.Durable.Get(1) {
		t.Error("article 1 is still durable although its bytes were overwritten")
	}
	if !got.Durable.Get(0) {
		t.Error("article 0 was discarded although its bytes still match their CRC")
	}
}

// TestResume_RecomputeYieldsAPrefixCRC pins R24: verification and CRC
// recovery are the same read, so a recomputed file gets a real CRC.
func TestResume_RecomputeYieldsAPrefixCRC(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100)
	whole := append(append([]byte{}, a0...), a1...)
	if err := os.WriteFile(path, whole, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
	}); err != nil {
		t.Fatal(err)
	}
	// No committed extent at all — forces the recompute path.
	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasPrefixCRC {
		t.Fatal("HasPrefixCRC = false after a full recompute over a gapless file")
	}
	if got.PrefixCRC != crc32.ChecksumIEEE(whole) {
		t.Errorf("PrefixCRC = %#x, want %#x", got.PrefixCRC, crc32.ChecksumIEEE(whole))
	}
	if got.VerifiedTo != 200 {
		t.Errorf("VerifiedTo = %d, want 200", got.VerifiedTo)
	}
	if !got.Recomputed {
		t.Error("Recomputed = false although no extent was ever committed")
	}
}

// TestResume_ArticleWithoutACRCIsLeftOutstanding pins S3's conservative
// default for an article whose bytes do not match what was recorded about them.
//
// Recomputation cannot prove those bytes are the right bytes, and the design's
// answer is that absence of evidence is absence: leave the article Outstanding
// and re-fetch it. Re-fetching costs one article; assuming is the optimism §1
// forbids.
//
// This used to be about UU-encoded articles, which carried no CRC at all and
// were skipped by a HasCRC guard. That guard is gone — every decode path now
// produces a checksum — so the only way an article fails to verify is a CRC
// that does not match, which is what the fixture below builds.
func TestResume_ArticleWhoseBytesDoNotMatchItsCRCIsLeftOutstanding(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")

	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100) // recorded under a CRC these bytes do not have
	if err := os.WriteFile(path, append(append([]byte{}, a0...), a1...), 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Durable.Get(0) {
		t.Error("article 0 is not durable, but its bytes match its recorded CRC")
	}
	if got.Durable.Get(1) {
		t.Error("article 1 is durable, but its bytes do not hash to its recorded CRC")
	}
	// The prefix stops at the unverifiable article, so no whole-file CRC is
	// reportable. Unavailable must be distinguishable from a CRC of zero (R23).
	if got.VerifiedTo != 100 {
		t.Errorf("VerifiedTo = %d, want 100 — the prefix cannot cross an article that failed to verify", got.VerifiedTo)
	}
	if got.HasPrefixCRC {
		t.Error("HasPrefixCRC = true, but the prefix ends at an article that failed to verify")
	}
	if got.Restart {
		t.Error("Restart = true for a file that exists and partly verified")
	}
}

// TestResume_MissingFileRestarts pins that a deleted partial starts over
// rather than resuming against nothing.
func TestResume_MissingFileRestarts(t *testing.T) {
	ctx := context.Background()
	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, filepath.Join(t.TempDir(), "gone.mkv"), 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Restart {
		t.Fatal("Restart = false for a file that does not exist")
	}
	if got.Durable.Count() != 0 {
		t.Error("a restarted file has durable articles")
	}
	if got.Durable.Len() != 4 {
		t.Errorf("Durable.Len() = %d, want 4 — a restarted file still needs a bitmap of the right width", got.Durable.Len())
	}
}

// TestResume_PrefixStopsAtAGap pins that the walk requires each fact to abut
// the run so far. Both articles here verify against their CRCs, so nothing but
// the offset comparison keeps VerifiedTo off the far one: a walk that skipped
// the gap would report 300 verified bytes for a file with a 100-byte hole,
// and PrefixCRC would then be the CRC of a range that is not [0, VerifiedTo).
func TestResume_PrefixStopsAtAGap(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")

	a0 := bytes.Repeat([]byte{0x01}, 100)
	a2 := bytes.Repeat([]byte{0x03}, 100)
	whole := append(append(append([]byte{}, a0...), make([]byte, 100)...), a2...)
	if err := os.WriteFile(path, whole, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: crc32.ChecksumIEEE(a2)},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.VerifiedTo != 100 {
		t.Errorf("VerifiedTo = %d, want 100 — the walk crossed the hole at [100, 200)", got.VerifiedTo)
	}
	if got.PrefixCRC != crc32.ChecksumIEEE(a0) {
		t.Errorf("PrefixCRC = %#x, want %#x — the CRC must cover exactly [0, VerifiedTo)",
			got.PrefixCRC, crc32.ChecksumIEEE(a0))
	}
	if got.HasPrefixCRC {
		t.Error("HasPrefixCRC = true although the prefix stops short of the file's end")
	}
	// The article above the hole is still durable: resume reads the bitmap,
	// not the anchor, so a hole costs a CRC and not a re-fetch (#311/#353).
	if !got.Durable.Get(2) {
		t.Error("article 2 was re-marked outstanding because of a hole below it")
	}
}

// TestResume_PrefixCRCUnavailableWhenTheFileExtendsBeyondTheFacts pins the
// other half of the whole-file rule. Every recorded fact verifies and they
// tile [0, 100) with no gap, so the prefix CRC is a true CRC of that prefix —
// but the file is 300 bytes long, so it is not a CRC of the file. R23 wants
// unavailable here, not a shorter range's CRC relabelled as the whole.
func TestResume_PrefixCRCUnavailableWhenTheFileExtendsBeyondTheFacts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")

	a0 := bytes.Repeat([]byte{0x01}, 100)
	if err := os.WriteFile(path, append(append([]byte{}, a0...), make([]byte, 200)...), 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.VerifiedTo != 100 {
		t.Errorf("VerifiedTo = %d, want 100", got.VerifiedTo)
	}
	if got.HasPrefixCRC {
		t.Error("HasPrefixCRC = true although the verified prefix covers only 100 of the file's 300 bytes")
	}
}

// TestResume_FactBeyondTheArticleCountFailsLoudly pins R28 against the
// article-numbering gap this signature carries: Resume is handed an article
// count but no global-to-file-local ordinal mapping, so it can only treat a
// fact's ArtIdx as the bit position. A fact outside that range proves the
// assumption is broken for this job. Setting no bit would be indistinguishable
// from "that article is not on disk" and would silently re-fetch a whole file.
func TestResume_FactBeyondTheArticleCountFailsLoudly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	if err := os.WriteFile(path, a0, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 7, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	_, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err == nil {
		t.Fatal("Resume returned no error for a fact whose article index is outside the file's article count")
	}
	if !errors.Is(err, ErrArticleOutOfRange) {
		t.Errorf("err = %v, want one wrapping ErrArticleOutOfRange", err)
	}
}

// TestResume_StatErrorIsReturned pins that a stat failure other than "not
// found" is surfaced rather than read as a missing file. A2 forbids
// log-and-continue, and restarting a file because its parent directory is
// unreadable would discard bytes that are still there.
func TestResume_StatErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	notADir := filepath.Join(dir, "regular")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, filepath.Join(notADir, "movie.mkv"), 0, 2)
	if err == nil {
		t.Fatalf("Resume returned no error for an unstattable path: %+v", got)
	}
	if got.Restart {
		t.Error("Restart = true for a path that failed to stat for a reason other than absence")
	}
}

// cancelAfterForFile cancels the resume's context the moment the fact log has
// answered, so the cancellation lands inside the verification loop rather than
// in the query that precedes it.
//
// Cancelling before the call instead is what the first version of the test did,
// and it proved nothing: SQLite's QueryContext fails on a dead context, so the
// error came back wrapping context.Canceled whether or not the loop checked at
// all — observed, by deleting the loop's check and watching the test stay
// green.
type cancelAfterForFile struct {
	FactLog
	cancel context.CancelFunc
}

func (c cancelAfterForFile) ForFile(ctx context.Context, jobID string, fileIdx int32) ([]ArticleFact, error) {
	facts, err := c.FactLog.ForFile(ctx, jobID, fileIdx)
	c.cancel()
	return facts, err
}

// TestResume_CancelledContextStops pins R15's interruptibility: a
// recomputation that is reading gigabytes must stop when its caller does,
// rather than run to completion after a shutdown.
func TestResume_CancelledContextStops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	if err := os.WriteFile(path, a0, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(context.Background(), "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewResumer(cancelAfterForFile{FactLog: fl, cancel: cancel}, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v (result %+v), want one wrapping context.Canceled", err, got)
	}
}

// TestResume_PrefixCRCCombinesAcrossThreeArticles guards the accumulator
// itself: with two articles a combine that returned its second argument would
// still produce the right answer at the end.
func TestResume_PrefixCRCCombinesAcrossThreeArticles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 50)
	a2 := bytes.Repeat([]byte{0x03}, 70)
	whole := bytes.Join([][]byte{a0, a1, a2}, nil)
	if err := os.WriteFile(path, whole, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 50, CRC32: crc32.ChecksumIEEE(a1)},
		{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 70, CRC32: crc32.ChecksumIEEE(a2)},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := crc32util.Combine(crc32util.Combine(crc32.ChecksumIEEE(a0), crc32.ChecksumIEEE(a1), 50),
		crc32.ChecksumIEEE(a2), 70)
	if want != crc32.ChecksumIEEE(whole) {
		t.Fatalf("fixture is wrong: combined %#x != whole %#x", want, crc32.ChecksumIEEE(whole))
	}
	if !got.HasPrefixCRC || got.PrefixCRC != want {
		t.Errorf("PrefixCRC = %#x (has=%v), want %#x", got.PrefixCRC, got.HasPrefixCRC, want)
	}
	if got.Durable.Count() != 3 {
		t.Errorf("Durable.Count() = %d, want 3", got.Durable.Count())
	}
}

// TestResume_SizeChangeWithPreservedMtimeForcesRecompute isolates the size
// half of the S7 stamp.
//
// The truncation test cannot do it: truncating also moves the mtime, so the
// mtime clause alone still catches that file and a guard that dropped the size
// check would keep the suite green. Restoring the mtime after the truncation
// is what leaves only the size to notice — and it is not contrived, since
// every timestamp-preserving restore (rsync --times, cp -p, tar extraction,
// a backup rollback) reinstates a partial file's old mtime over content of a
// different length.
func TestResume_SizeChangeWithPreservedMtimeForcesRecompute(t *testing.T) {
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
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(2)
	bm.Set(0)
	bm.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 200, Size: 200, ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	if err := os.Truncate(path, 100); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.ModTime().UnixNano() != fi.ModTime().UnixNano() {
		t.Fatalf("fixture is wrong: mtime moved (%d -> %d), so this no longer isolates the size clause",
			fi.ModTime().UnixNano(), after.ModTime().UnixNano())
	}

	r := NewResumer(fl, es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Recomputed {
		t.Fatal("Recomputed = false after a truncation that preserved the mtime — only the size moved and the cache was adopted")
	}
	if got.Durable.Get(1) {
		t.Error("article 1 is still durable after its bytes were truncated away")
	}
}

// TestResume_FastPathWidensANarrowerStoredBitmap pins the other direction of
// the same re-derivation. A stored bitmap can be narrower than the file's
// article count — an older commit, or a manifest that grew — and re-deriving
// it at artCount without first zero-padding the buffer fails outright rather
// than reading the missing articles as "not durable yet", which is the safe
// direction under S3.
func TestResume_FastPathWidensANarrowerStoredBitmap(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(2)
	bm.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: bm, Size: 300, ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 200)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.Durable.Len() != 200 {
		t.Errorf("Durable.Len() = %d, want 200", got.Durable.Len())
	}
	if !got.Durable.Get(1) || got.Durable.Count() != 1 {
		t.Errorf("widening lost or invented bits: count=%d", got.Durable.Count())
	}
}

// TestGaplessPrefixCRC exercises the walk directly, over the fact/verified
// shapes that are awkward to reach through Resume: an empty fact list, a
// verified run that stops because the next fact was not proven, and one that
// stops because the next fact does not abut it.
func TestGaplessPrefixCRC(t *testing.T) {
	a := bytes.Repeat([]byte{0x01}, 100)
	b := bytes.Repeat([]byte{0x02}, 100)
	crcA, crcB := crc32.ChecksumIEEE(a), crc32.ChecksumIEEE(b)
	abut := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 1, Offset: 100, Length: 100, CRC32: crcB},
	}
	gapped := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 2, Offset: 200, Length: 100, CRC32: crcB},
	}

	tests := []struct {
		name      string
		facts     []ArticleFact
		verified  []bool
		size      int64
		wantTo    int64
		wantCRC   uint32
		wantWhole bool
	}{
		{
			name: "no facts and no bytes is a vacuously whole empty file",
			size: 0, wantWhole: true,
		},
		{
			name: "no facts but bytes on disk is not whole",
			size: 100,
		},
		{
			name:  "both verified and abutting reaches the end",
			facts: abut, verified: []bool{true, true}, size: 200,
			wantTo: 200, wantCRC: crc32util.Combine(crcA, crcB, 100), wantWhole: true,
		},
		{
			name:  "an unverified second fact stops the run short",
			facts: abut, verified: []bool{true, false}, size: 200,
			wantTo: 100, wantCRC: crcA,
		},
		{
			name:  "a verified fact that does not abut stops the run",
			facts: gapped, verified: []bool{true, true}, size: 300,
			wantTo: 100, wantCRC: crcA,
		},
		{
			name:  "an unverified first fact yields nothing",
			facts: abut, verified: []bool{false, true}, size: 200,
		},
		{
			name:  "reaching the end with a fact left over is not whole",
			facts: abut, verified: []bool{true, false}, size: 100,
			wantTo: 100, wantCRC: crcA,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			to, crc, whole := gaplessPrefixCRC(tc.facts, tc.verified, tc.size)
			if to != tc.wantTo || crc != tc.wantCRC || whole != tc.wantWhole {
				t.Errorf("gaplessPrefixCRC = (%d, %#x, %v), want (%d, %#x, %v)",
					to, crc, whole, tc.wantTo, tc.wantCRC, tc.wantWhole)
			}
		})
	}
}

// TestResumerCommittedExtent exercises the lookup directly: the "no extent for
// this job" and "extents exist but not for this file" cases both have to
// report absence rather than another file's cache.
func TestResumerCommittedExtent(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(3)
	bm.Set(2)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 1, Durable: bm, VerifiedTo: 42, Size: 99, ModTimeNs: 7,
	}}); err != nil {
		t.Fatal(err)
	}
	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), es, testLogger(t))

	if _, ok, err := r.committedExtent(ctx, "job-2", 1, 3); err != nil || ok {
		t.Errorf("committedExtent for an unknown job = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	if _, ok, err := r.committedExtent(ctx, "job-1", 0, 3); err != nil || ok {
		t.Errorf("committedExtent for an uncommitted file = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	got, ok, err := r.committedExtent(ctx, "job-1", 1, 3)
	if err != nil || !ok {
		t.Fatalf("committedExtent = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
	if got.VerifiedTo != 42 || got.Size != 99 || got.ModTimeNs != 7 {
		t.Errorf("committedExtent lost fields: %+v", got)
	}
	if got.Durable.Len() != 3 || !got.Durable.Get(2) || got.Durable.Count() != 1 {
		t.Errorf("committedExtent bitmap = len %d count %d, want len 3 count 1",
			got.Durable.Len(), got.Durable.Count())
	}
}

// TestResumerVerifyRegions exercises the read loop directly, because the
// parallel []bool it returns is the only link between verification and the
// prefix walk and is invisible from Resume's result.
func TestResumerVerifyRegions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100)
	if err := os.WriteFile(path, append(append([]byte{}, a0...), a1...), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	facts := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a0)}, // wrong CRC
		{ArtIdx: 2, Offset: 200, Length: 100, CRC32: crc32.ChecksumIEEE(a1)}, // past the end
		{ArtIdx: 3, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)}, // matches
	}
	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	durable := NewBitmap(4)
	verified, err := r.verifyRegions(ctx, f, facts, durable, 200, 0, 4, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, false, false, true}
	for i := range want {
		if verified[i] != want[i] {
			t.Errorf("verified[%d] = %v, want %v", i, verified[i], want[i])
		}
		if durable.Get(i) != want[i] {
			t.Errorf("durable bit %d = %v, want %v", i, durable.Get(i), want[i])
		}
	}
}

// TestResumerRecompute exercises the recompute path directly, pinning that it
// is what sets Recomputed and that a negative offset is refused rather than
// read.
func TestResumerRecompute(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	if err := os.WriteFile(path, a0, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: -1, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
	}); err != nil {
		t.Fatal(err)
	}
	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.recompute(ctx, "job-1", 0, path, 100, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Recomputed {
		t.Error("Recomputed = false from recompute itself")
	}
	if got.Durable.Count() != 0 {
		t.Error("a fact at a negative offset was treated as durable")
	}
	if _, err := r.recompute(ctx, "job-1", 0, filepath.Join(t.TempDir(), "gone"), 100, 0, 2); err == nil {
		t.Error("recompute returned no error for a file it could not open")
	}
}

// TestResume_FastPathKeepsAWholeFilePrefixCRC is the positive half of the
// re-check: when the cached prefix does cover the whole file, the stamp
// matching is enough and the CRC is adopted. Without this the gate could be
// satisfied by never adopting a CRC at all, which would quietly cost R24 the
// short-circuit it exists for.
func TestResume_FastPathKeepsAWholeFilePrefixCRC(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(4)
	bm.Set(0)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 300, PrefixCRC: 0x1234, HasPrefixCRC: true,
		Size: 300, ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recomputed {
		t.Error("Recomputed = true with a matching stamp")
	}
	if !got.HasPrefixCRC || got.PrefixCRC != 0x1234 {
		t.Errorf("a whole-file cached CRC was dropped: has=%v crc=%#x", got.HasPrefixCRC, got.PrefixCRC)
	}
}

// TestResume_FastPathDropsAPrefixCRCThatNoLongerSpansTheFile pins the sequence
// that makes the re-check necessary rather than defensive.
//
// A resume commits HasPrefixCRC over a whole file. An article then lands beyond
// a hole: the file grows, VerifiedTo does not move, and the barrier's clear
// only fires when VerifiedTo *changes* — so the flag survives, now describing a
// prefix that is no longer the file. The stamp still matches on the next
// restart, because the stamp is about external modification and this change was
// ours. Adopting the flag here would report a partial extent's CRC as the
// file's: #349 re-entering through the cache instead of the walk.
func TestResume_FastPathDropsAPrefixCRCThatNoLongerSpansTheFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")

	// The file as it stood when the CRC was true of all of it.
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 200), 0o644); err != nil {
		t.Fatal(err)
	}
	// An article lands beyond a hole: the file grows, the verified prefix
	// does not.
	if err := os.Truncate(path, 400); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(4)
	bm.Set(0)
	bm.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 200, PrefixCRC: 0xFEED, HasPrefixCRC: true,
		Size: fi.Size(), ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recomputed {
		t.Fatal("Recomputed = true — the stamp matched, so this is not the recompute path's job")
	}
	if got.HasPrefixCRC {
		t.Error("HasPrefixCRC = true: a CRC over 200 of the file's 400 bytes was adopted as the file's")
	}
	// The done-set and the anchor are still adopted; only the claim about
	// what the CRC covers is refused.
	if got.Durable.Count() != 2 || got.VerifiedTo != 200 {
		t.Errorf("the rest of the cache was discarded with the flag: %+v", got)
	}
}

// TestResume_BitsLandAtTheFileLocalOrdinal pins the firstArtIdx mapping.
//
// FileExtent.Durable is indexed file-locally, so for the job's second file the
// global ArtIdx and the bit position differ by the file's first index. Reading
// the global index as the bit position is correct only for a job's first file,
// which is why every fixture with firstArtIdx == 0 is blind to it.
func TestResume_BitsLandAtTheFileLocalOrdinal(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "second.mkv")

	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100)
	if err := os.WriteFile(path, append(append([]byte{}, a0...), a1...), 0o644); err != nil {
		t.Fatal(err)
	}
	// File 1 owns global articles 10 and 11; its file-local ordinals are 0
	// and 1.
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 1, ArtIdx: 10, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 1, ArtIdx: 11, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 1, path, 10, 2)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !got.Durable.Get(0) || !got.Durable.Get(1) {
		t.Errorf("bits did not land at the file-local ordinals: %064b", got.Durable.Bytes())
	}
	if got.Durable.Count() != 2 {
		t.Errorf("Durable.Count() = %d, want 2", got.Durable.Count())
	}
	if got.VerifiedTo != 200 || !got.HasPrefixCRC {
		t.Errorf("VerifiedTo = %d has=%v, want 200 true", got.VerifiedTo, got.HasPrefixCRC)
	}
}

// TestResume_FactBelowTheFirstArticleFailsLoudly is the other side of the
// range guard: an index under firstArtIdx maps to a negative ordinal, which is
// as much a numbering defect as one over the count.
func TestResume_FactBelowTheFirstArticleFailsLoudly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "second.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	if err := os.WriteFile(path, a0, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 1, ArtIdx: 3, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
	}); err != nil {
		t.Fatal(err)
	}
	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	if _, err := r.Resume(ctx, "job-1", 1, path, 10, 2); !errors.Is(err, ErrArticleOutOfRange) {
		t.Errorf("err = %v, want one wrapping ErrArticleOutOfRange", err)
	}
}

// TestResume_RegionAtTheOffsetLimitDoesNotWrap pins the bounds guard against a
// corrupt row. Offset+Length would overflow into a negative number and sail
// past a "> size" comparison; ReadAt would then fail, so the visible outcome
// would be a loud error either way — but on a claim the guard had not actually
// checked. Refusing the region is the correct disposition: nothing about those
// bytes was proven.
func TestResume_RegionAtTheOffsetLimitDoesNotWrap(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	if err := os.WriteFile(path, a0, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 0, ArtIdx: 1, Offset: math.MaxInt64 - 10, Length: 100, CRC32: 0},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.Durable.Get(1) {
		t.Error("a region whose offset+length overflows was treated as verified")
	}
	if !got.Durable.Get(0) || got.VerifiedTo != 100 {
		t.Errorf("the intact article was lost with it: count=%d verifiedTo=%d",
			got.Durable.Count(), got.VerifiedTo)
	}
}

// TestResume_FastPathDoesNotInventAPrefixCRC guards the other half of the
// re-check's conjunction. An extent whose prefix happens to span the file but
// which never carried a CRC must not come back claiming one: "the prefix
// reaches the end" is a statement about VerifiedTo, not evidence that anything
// computed a checksum over it.
func TestResume_FastPathDoesNotInventAPrefixCRC(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	es := NewSQLiteExtentStore(openTestDB(t))
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: NewBitmap(4), VerifiedTo: 300, PrefixCRC: 0, HasPrefixCRC: false,
		Size: 300, ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.HasPrefixCRC {
		t.Error("HasPrefixCRC = true for an extent that was committed without one")
	}
}
