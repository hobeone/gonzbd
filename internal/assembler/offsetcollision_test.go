package assembler

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"testing"
)

// Offset collisions (#383, #379).
//
// Two articles claiming overlapping byte ranges resolve by whether the incumbent
// has written. Every accepted article is written synchronously, so an incumbent
// is on disk before a rival can arrive, with one exception: an article whose
// write faulted owns nothing.
//
//   - Incumbent written → the range is OWNED and the ARRIVAL is rejected.
//     Its bytes are on disk and the recorder holds the row naming its CRC at
//     that offset. Letting a later article overwrite the range makes that
//     record unverifiable, and failing the incumbent as well would give one article
//     two terminal dispositions. Checked in acceptArticle, refused like any
//     other article-level rejection.
//   - Incumbent faulted, never written → it claimed nothing, so the arrival is
//     accepted.

// --- Owned ranges: the arrival is rejected ---------------------------------

// collisionFixture drives two articles at one offset through the real accept
// path and reports what came out.
type collisionFixture struct {
	a        *Assembler
	f        *openFile
	rejected []int32
	// unwritten collects what OnArticlesUnwritten was told to return to
	// Outstanding, which is how a rolled-back article is observed.
	unwritten []int32
}

func newCollisionFixture(t *testing.T) *collisionFixture {
	t.Helper()
	c := &collisionFixture{a: newHelperAssembler()}
	c.a.opts.OnArticleRejected = func(_ Owner, _ int, artIdx int32, _ string, _ int64) {
		c.rejected = append(c.rejected, artIdx)
	}
	c.a.opts.OnArticlesUnwritten = func(_ Owner, _ int, arts []int32) {
		c.unwritten = append(c.unwritten, arts...)
	}
	c.f = newHelperFile(t, t.TempDir(), "collide.dat", 1<<20)
	c.f.info.TotalParts = 2
	return c
}

// accept drives one article through handleSuccessArticle and reports whether
// it was counted toward the file's part total.
func (c *collisionFixture) accept(artIdx int32, msgID string, off int64, data []byte) bool {
	before := c.f.w.parts()
	c.a.handleSuccessArticle(c.f, WriteRequest{
		Owner: ownerFor("job"), JobID: "job", FileIdx: 0, ArtIdx: artIdx, MessageID: msgID,
		Offset: off, Data: data,
	})
	return c.f.w.parts() > before
}

// TestCollision_ArrivalRejectedOnceIncumbentIsWritten is the #383 pin: an
// arrival at an offset whose incumbent has been written is refused, and the
// incumbent's bytes stay the truth at that offset.
func TestCollision_ArrivalRejectedOnceIncumbentIsWritten(t *testing.T) {
	c := newCollisionFixture(t)

	if !c.accept(1, "<first@x>", 0, []byte("AAAA")) {
		t.Fatal("precondition: the incumbent was not counted")
	}
	counted := c.accept(2, "<second@x>", 0, []byte("BBBB"))

	if !counted {
		t.Error("the rejected arrival was not counted toward the file's part total")
	}
	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — the arrival loses a written offset", c.rejected)
	}
	if len(c.unwritten) != 0 {
		t.Errorf("the written incumbent was returned to Outstanding as well as remaining in the "+
			"written set: %v — one article, two terminal dispositions", c.unwritten)
	}
	onDisk, err := os.ReadFile(c.f.w.path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if len(onDisk) < 4 || string(onDisk[:4]) != "AAAA" {
		t.Errorf("bytes at offset 0 = %q, want the incumbent's AAAA", onDisk)
	}
}

// TestCollision_RangeStaysOwnedAfterSync pins that a successful sync does not
// release a range. An article whose bytes an fsync has covered holds the
// strongest possible claim on its offset, so a collision arriving after the
// fsync must still be refused rather than overwrite an article the recorder
// has already described.
func TestCollision_RangeStaysOwnedAfterSync(t *testing.T) {
	c := newCollisionFixture(t)

	if !c.accept(1, "<first@x>", 0, []byte("AAAA")) {
		t.Fatal("precondition: the incumbent was not counted")
	}
	if err := c.f.w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	c.accept(2, "<second@x>", 0, []byte("BBBB"))

	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — after the Sync the incumbent's "+
			"bytes are covered by an fsync, and overwriting them now contradicts a fact the "+
			"recorder has already described", c.rejected)
	}
	if len(c.unwritten) != 0 {
		t.Errorf("an already-acked article was returned to Outstanding: %v", c.unwritten)
	}
}

// TestOwnedRanges_ArrivalVerdicts covers the predicate that decides whether an
// arrival is refused, directly, including the ways it must answer "no".
func TestOwnedRanges_ArrivalVerdicts(t *testing.T) {
	owner := articleID{artIdx: 1}

	tests := []struct {
		name      string
		seed      func(w *fileWriter)
		r         Range
		wantOwned bool
	}{
		{"an unclaimed range is not owned", func(*fileWriter) {}, Range{0, 4}, false},
		{"the same range is owned", func(w *fileWriter) { w.owned.claim(Range{0, 4}, owner) }, Range{0, 4}, true},
		{"a partial overlap is owned", func(w *fileWriter) { w.owned.claim(Range{0, 4}, owner) }, Range{2, 4}, true},
		{"a partial overlap into a range's head is owned", func(w *fileWriter) { w.owned.claim(Range{1000, 1000}, owner) }, Range{900, 1000}, true},
		{"an abutting range is not owned", func(w *fileWriter) { w.owned.claim(Range{0, 4}, owner) }, Range{4, 4}, false},
		{"a range abutting from below is not owned", func(w *fileWriter) { w.owned.claim(Range{1000, 1000}, owner) }, Range{0, 1000}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestFileWriter(t)
			tc.seed(w)

			got, owned := w.owned.ownerOf(tc.r)

			if owned != tc.wantOwned {
				t.Fatalf("owned = %v, want %v", owned, tc.wantOwned)
			}
			if owned && got != owner {
				t.Errorf("owner = %+v, want %+v — the caller names it in the warning", got, owner)
			}
		})
	}
}

// TestCollision_RetryAfterWriteFaultIsAccepted drives a write fault and the
// faulted article's redelivery through handleSuccessArticle. The faulted write
// claimed nothing, so the retry at the same offset finds no owner and is
// written, rather than being refused as a collision with nothing.
func TestCollision_RetryAfterWriteFaultIsAccepted(t *testing.T) {
	c := newCollisionFixture(t)
	realWrite := c.f.w.writeAt
	c.f.w.writeAt = func([]byte, int64) (int, error) { return 0, errors.New("injected write fault") }
	if c.accept(7, "<retry@x>", 0, bytes.Repeat([]byte{'A'}, 64)) {
		t.Fatal("precondition: the faulted write was counted")
	}
	if !slices.Equal(c.unwritten, []int32{7}) {
		t.Fatalf("precondition: OnArticlesUnwritten = %v, want [7]", c.unwritten)
	}

	c.f.w.writeAt = realWrite
	if !c.accept(7, "<retry@x>", 0, bytes.Repeat([]byte{'A'}, 64)) {
		t.Error("the retry was not counted toward the file's part total")
	}
	if len(c.rejected) != 0 {
		t.Errorf("OnArticleRejected = %v, want none: the faulted write owned no range", c.rejected)
	}
	if got := ownedBy(c.f.w, 7); !slices.Equal(got, []Range{{0, 64}}) {
		t.Errorf("article 7 owns %v after the retry, want [{0 64}]", got)
	}
}

// TestFileWriter_RangeOwnersStayAdmitted pins the invariant that lets
// ownedRanges own a range against every arrival: an article that owns a range
// is in its writer's seenDone for the writer's life, so admit drops its
// redelivery before acceptArticle consults the range set.
//
// It drives every disposition, and a seeded range, through processRequest and
// checks the invariant after each step. If the invariant broke, the owner's
// redelivery would reach ownerOf, find its own range, and be refused as a
// collision with itself.
func TestFileWriter_RangeOwnersStayAdmitted(t *testing.T) {
	a := newHelperAssembler()
	var rejected, unwritten []int32
	a.opts.OnArticleRejected = func(_ Owner, _ int, artIdx int32, _ string, _ int64) {
		rejected = append(rejected, artIdx)
	}
	a.opts.OnArticlesUnwritten = func(_ Owner, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}
	f := newHelperFile(t, t.TempDir(), "owners.dat", 1<<20)
	f.info.TotalParts = 100 // never completes, so the writer outlives every step
	a.seedOwned(f.w, []Range{{Off: 100, Len: 10}})
	open := map[fileKey]*openFile{{jobID: "job", fileIdx: 0}: f}
	completed := map[fileKey]struct{}{}
	send := func(artIdx int32, off int64, payload string, fatal error) {
		var data []byte
		if fatal == nil {
			data = []byte(payload)
		}
		a.processRequest(WriteRequest{
			Owner: ownerFor("job"), JobID: "job", FileIdx: 0, ArtIdx: artIdx, MessageID: "m@x",
			Offset: off, Data: data, FatalErr: fatal,
		}, open, completed)
	}
	check := func(step string) {
		t.Helper()
		for _, o := range f.w.owned.s {
			if o.id == seededOwner {
				continue
			}
			if _, ok := f.w.seenDone[o.id.artIdx]; !ok {
				t.Fatalf("after %s: article %d owns %+v but is not in seenDone, so its "+
					"redelivery would reach the overlap check", step, o.id.artIdx, o.r)
			}
		}
	}

	send(0, 0, "AAAA", nil)
	check("0 written")
	realWrite := f.w.writeAt
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, errors.New("injected write fault") }
	send(1, 4, "BBBB", nil)
	f.w.writeAt = realWrite
	check("1 rolled back")
	send(1, 4, "BBBB", nil)
	check("1 written on retry")
	send(2, 2, "CCCC", nil)
	check("2 rejected over 0 and 1")
	send(3, -1, "DDDD", nil)
	check("3 rejected out of range")
	send(4, 102, "EEEE", nil)
	check("4 rejected over the seeded range")
	send(5, 0, "", errFatalProbe)
	check("5 fetch-failed")
	send(0, 0, "", errFatalProbe)
	check("0 fetch-failed after being written")
	send(0, 0, "ZZZZ", nil)
	check("0 redelivered")
	send(1, 4, "ZZZZ", nil)
	check("1 redelivered")

	if !slices.Equal(rejected, []int32{2, 3, 4}) {
		t.Errorf("OnArticleRejected = %v, want [2 3 4]: a redelivered owner was refused "+
			"against its own range", rejected)
	}
	if !slices.Equal(unwritten, []int32{1}) {
		t.Errorf("OnArticlesUnwritten = %v, want [1]", unwritten)
	}
	got, err := os.ReadFile(f.w.path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got[:8]) != "AAAABBBB" {
		t.Errorf("bytes [0,8) = %q, want AAAABBBB: a redelivery rewrote an owned range", got[:8])
	}
}

// TestFileWriter_FaultedWriteOwnsNothing pins what a write fault leaves
// behind: the article loses its part and its seenDone entry and, because its
// range is claimed only after the write returned nil, owns nothing. A different
// article at the same range is accepted, and once written it owns the range.
func TestFileWriter_FaultedWriteOwnsNothing(t *testing.T) {
	w := newTestFileWriter(t)
	first := articleID{artIdx: 1}
	second := articleID{artIdx: 2}
	r := Range{0, 4}

	realWrite := w.writeAt
	w.writeAt = func([]byte, int64) (int, error) { return 0, errors.New("injected write fault") }
	w.admit(first.artIdx)
	if err := w.accept(first, 0, []byte("AAAA")); err == nil {
		t.Fatal("precondition: the injected write fault did not surface")
	}
	w.resolve(first.artIdx, dispositionRolledBack)
	if w.parts() != 0 {
		t.Fatalf("parts = %d after the rollback, want 0", w.parts())
	}
	if owner, owned := w.owned.ownerOf(r); owned {
		t.Fatalf("ownerOf = %+v after a faulted write, want no owner", owner)
	}

	w.writeAt = realWrite
	w.admit(second.artIdx)
	if err := w.accept(second, 0, []byte("BBBB")); err != nil {
		t.Fatalf("accept second: %v", err)
	}
	if got, owned := w.owned.ownerOf(r); !owned || got != second {
		t.Errorf("ownerOf = %+v, %v; want the second article to own it", got, owned)
	}
}
