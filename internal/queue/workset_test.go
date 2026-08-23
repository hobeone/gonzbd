package queue

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// stubSyncTarget is a durability.SyncTarget with a fixed answer. It reports
// one file whose articles are already written, so a barrier run over it does
// no I/O and reaches the ack.
type stubSyncTarget struct {
	confirmed []int32
	files     []int32
	written   []durability.WrittenArticle
}

func (s *stubSyncTarget) Files() []int32    { return s.files }
func (s *stubSyncTarget) Path(int32) string { return "/downloads/stub.bin" }
func (s *stubSyncTarget) Drain(context.Context, int32) ([]durability.WrittenArticle, error) {
	return s.written, nil
}
func (s *stubSyncTarget) Sync(context.Context, int32) error { return nil }
func (s *stubSyncTarget) Stat(int32) (int64, error)         { return 4096, nil }

// ackerFunc is a func-typed durability.Acker.
type ackerFunc func(durability.DurableProof) error

func (f ackerFunc) AckDurable(p durability.DurableProof) error { return f(p) }

// noopStallable satisfies durability.Stallable. A barrier run in these tests
// never faults, so neither method should fire.
type noopStallable struct{}

func (noopStallable) Stall(string, *storagefault.Fault) {}
func (noopStallable) Fail(string, *storagefault.Fault)  {}

// mintProof produces a DurableProof the way production does: by running a real
// durability.Barrier over a stub target and capturing what it emits.
//
// There is deliberately no shortcut. DurableProof has no exported constructor,
// so no package outside internal/durability can build one that NAMES AN
// ARTICLE, and that absence is what makes "ack only after fsync" enforced by
// the compiler on this path rather than by a rule six call sites must each
// remember. A test-only exported constructor would move that guarantee to a CI
// grep, so this helper pays the setup cost instead — and gets a test that
// exercises the real minting path as a bonus.
//
// Note the bound, which an earlier version of this comment overstated:
// durability.DurableProof{} does compile here, it is simply empty and inert
// (TestAckDurable_ExternallyConstructibleEmptyProofAcksNothing), and
// AckDurable is not the only door into markDone — SeedFromRuns and
// ReplaceFromRuns need no proof at all.
func mintProof(t *testing.T, jobID string, arts []int32) durability.DurableProof {
	t.Helper()

	written := make([]durability.WrittenArticle, len(arts))
	for i, a := range arts {
		written[i] = durability.WrittenArticle{
			FileIdx: 0, ArtIdx: a, Offset: int64(a) * 100, Length: 100,
		}
	}
	tgt := &stubSyncTarget{files: []int32{0}, written: written}

	var got durability.DurableProof
	captured := ackerFunc(func(p durability.DurableProof) error { got = p; return nil })

	hdb, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "mint.db"))
	if err != nil {
		t.Fatalf("mintProof: open db: %v", err)
	}
	t.Cleanup(func() { _ = hdb.Close() })
	db := history.NewRepository(hdb).DB()

	b := durability.NewBarrier(
		durability.NewSQLiteRunStore(db),
		captured,
		noopStallable{},
		slog.New(slog.DiscardHandler),
	)
	if _, err := b.Run(context.Background(), jobID, tgt); err != nil {
		t.Fatalf("mintProof: barrier run: %v", err)
	}
	if len(got.Articles()) != len(arts) {
		t.Fatalf("mintProof: barrier emitted %d articles, want %d", len(got.Articles()), len(arts))
	}
	return got
}

// newTestQueueWithJob builds a resident single-file job of n articles.
func newTestQueueWithJob(t *testing.T, jobID string, n int) *Queue {
	t.Helper()
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, jobID, 1, n)
	job.ID = jobID
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return q
}

// outstandingFor lists the job's articles that are still to fetch.
func outstandingFor(q *Queue, jobID string) []int32 {
	var out []int32
	q.mu.RLock()
	defer q.mu.RUnlock()
	job := q.byID[jobID]
	for i := range job.manifest.NumArticles() {
		if !job.progress.ArticleDone(i) {
			out = append(out, int32(i)) //nolint:gosec // G115: test article counts are tiny
		}
	}
	return out
}

func TestAckDurable_MarksArticlesResolved(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 10)
	p := mintProof(t, "job-1", []int32{0, 3, 7})

	if err := q.AckDurable(p); err != nil {
		t.Fatal(err)
	}
	outstanding := outstandingFor(q, "job-1")
	for _, resolved := range []int32{0, 3, 7} {
		if slices.Contains(outstanding, resolved) {
			t.Errorf("article %d still outstanding after AckDurable", resolved)
		}
	}
	if len(outstanding) != 7 {
		t.Errorf("outstanding = %d, want 7", len(outstanding))
	}
}

// TestAckDurable_IsIdempotent pins R12: at-least-once delivery with an
// idempotent apply. SyncTarget.Drain is explicitly permitted to re-report an
// article a previous Drain returned, so a replayed proof must not double-count
// bytes.
func TestAckDurable_IsIdempotent(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 10)
	p := mintProof(t, "job-1", []int32{0, 1, 2})
	if err := q.AckDurable(p); err != nil {
		t.Fatal(err)
	}
	before := q.SnapshotJob("job-1").Progress().FileBytesDownloaded(0)
	if before == 0 {
		t.Fatal("fixture charged no bytes; the replay check would pass vacuously")
	}
	if err := q.AckDurable(p); err != nil {
		t.Fatal(err)
	}
	if after := q.SnapshotJob("job-1").Progress().FileBytesDownloaded(0); after != before {
		t.Fatalf("bytes = %d after a replayed proof, want %d — the apply is not idempotent", after, before)
	}
}

// TestAckDurable_OutOfRangeArticleDoesNotAbortTheBatch restores the
// out-of-bounds coverage the deleted MarkArticlesDoneByIdx carried.
//
// The in-range articles were made durable by a real fsync, so dropping their
// acks because a sibling index was malformed would cost a re-download of bytes
// already on disk. The out-of-range one is a numbering defect upstream, and it
// is logged rather than silently ignored (A2).
func TestAckDurable_OutOfRangeArticleDoesNotAbortTheBatch(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 4)
	// artCount 8 lets the barrier place article 6, which the 4-article job
	// does not have — the mismatch this test is about.
	p := mintProof(t, "job-1", []int32{0, 6})

	if err := q.AckDurable(p); err != nil {
		t.Fatalf("AckDurable aborted the whole batch for one bad index: %v", err)
	}
	outstanding := outstandingFor(q, "job-1")
	if slices.Contains(outstanding, 0) {
		t.Error("article 0 was not resolved, although its bytes were durable")
	}
	if len(outstanding) != 3 {
		t.Errorf("outstanding = %d, want 3 — the out-of-range index must resolve nothing", len(outstanding))
	}
}

// TestQueue_HasNoNonBarrierAckPath pins X2 structurally. If a future change
// reintroduces a public ack that does not require a DurableProof, this fails.
func TestQueue_HasNoNonBarrierAckPath(t *testing.T) {
	forbidden := []string{
		"MarkArticleDone", "MarkArticleFailed",
		"MarkArticlesDone", "MarkArticlesDoneByIdx",
		"MarkArticlesFailed", "MarkArticlesFailedByIdx",
		"SetFileExtents",
	}
	qt := reflect.TypeFor[*Queue]()
	for method := range qt.Methods() {
		name := method.Name
		if slices.Contains(forbidden, name) {
			t.Errorf("Queue.%s still exists — X2 requires the barrier to be the only ack path", name)
		}
	}
}

// TestAckDurable_RequiresAProof is the compiler half of X3, asserted as a type
// fact rather than as behaviour: the only parameter AckDurable accepts is a
// durability.DurableProof, whose zero value is useless and which no package
// outside internal/durability can construct with content.
func TestAckDurable_RequiresAProof(t *testing.T) {
	m, ok := reflect.TypeFor[*Queue]().MethodByName("AckDurable")
	if !ok {
		t.Fatal("Queue.AckDurable is missing — the barrier has no way to ack")
	}
	want := reflect.TypeFor[durability.DurableProof]()
	if got := m.Type.In(1); got != want {
		t.Errorf("AckDurable takes %v, want %v — a non-proof parameter reopens the "+
			"pre-barrier ack path that R9 exists to close", got, want)
	}
}

// TestSetWarning_IsReadableWithoutResidency pins the field R27's stall reason
// travels on.
//
// Header tier deliberately: the condition that sets it — a storage fault — is
// most likely to arrive when the job is in an unusual state, so a setter that
// needed the manifest or residency would drop the reason for exactly the jobs
// that have one. An unknown job is reported rather than silently ignored (A2).
func TestSetWarning_IsReadableWithoutResidency(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 4)

	if err := q.SetWarning("job-1", "Stalled: storage retryable fault on sync \"/mnt/x\": no space left on device"); err != nil {
		t.Fatalf("SetWarning: %v", err)
	}
	snap := q.SnapshotJob("job-1")
	if snap == nil {
		t.Fatal("job missing")
	}
	if !strings.Contains(snap.Warning, "no space left on device") {
		t.Errorf("Warning = %q, want the fault's text — the job halts with no reason a user can act on", snap.Warning)
	}
	if !q.IsDirty() {
		t.Error("the queue was not marked dirty, so the reason is lost on the next reload")
	}

	// Overwriting replaces rather than appends: the current reason is the one
	// that matters.
	if err := q.SetWarning("job-1", "Stalled: read-only filesystem"); err != nil {
		t.Fatal(err)
	}
	if got := q.SnapshotJob("job-1").Warning; got != "Stalled: read-only filesystem" {
		t.Errorf("Warning = %q after a second call, want only the newer reason", got)
	}

	if err := q.SetWarning("no-such-job", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetWarning on an unknown job = %v, want ErrNotFound", err)
	}
}

// ---------- ReplaceFromRuns (#362) ----------

// runsOf builds one single-article durable Run per named article of file 0.
//
// Single-article rows rather than merged spans: a run's [FirstArtIdx,
// LastArtIdx] range is all the queue's two seeding doors read, and merging
// here would only make the fixtures harder to read. Merging is RunStore's job
// and is pinned in internal/durability.
//
// The nArts parameter is unused by the construction and kept in the signature
// so each call site states the file's width beside the articles it names —
// runsCoverage refuses a run outside the file's manifest range, so a fixture
// whose two disagree fails loudly rather than silently seeding nothing.
func runsOf(set ...int) []durability.Run { return fileRunsOf(0, 0, set...) }

// fileRunsOf is runsOf for a file other than the first. base is the file's
// first GLOBAL article index, which is what converts the file-local numbers a
// fixture reads naturally into the global ones a Run carries.
func fileRunsOf(fileIdx int32, base int, set ...int) []durability.Run {
	runs := make([]durability.Run, 0, len(set))
	for _, i := range set {
		idx := int32(base + i) //nolint:gosec // G115: fixture article counts are tiny
		runs = append(runs, durability.Run{
			FileIdx: fileIdx, FirstArtIdx: idx, LastArtIdx: idx, Length: 1,
		})
	}
	return runs
}

// restoreDone puts a job into the state Store.RestoreJobProgress leaves it in:
// the named articles marked done because the durability record said so, with
// nothing on disk having been consulted.
//
// It goes through SeedFromRuns rather than reaching for markDone, so the
// fixture cannot drift from what the additive path actually does — and so a
// mutation that breaks markDone shows up as a fixture failure rather than as a
// silent agreement between fixture and subject.
func restoreDone(t *testing.T, q *Queue, jobID string, nArts int, done ...int) {
	t.Helper()
	_ = nArts
	if err := q.SeedFromRuns(jobID, runsOf(done...)); err != nil {
		t.Fatalf("restoreDone: SeedFromRuns: %v", err)
	}
	for _, i := range done {
		if !q.SnapshotJob(jobID).Progress().ArticleDone(i) {
			t.Fatalf("restoreDone: article %d is not Done after the restore; the fixture "+
				"never reached the state under test", i)
		}
	}
}

// TestReplaceFromRuns_ClearsArticlesTheResumerDidNotVerify is #362's core
// claim: a bit that was merely restored from job_files.articles_done loses to
// a resume that read the file's bytes and did not find the article there.
//
// The fixture claims MORE than the resume verifies (0,1,2 restored against 0,2
// verified) and keeps at least one article durable in both (0 and 2). Both
// halves are load-bearing: with nothing surviving, a clear-everything mutation
// would pass; with nothing cleared, a no-op would.
func TestReplaceFromRuns_ClearsArticlesTheResumerDidNotVerify(t *testing.T) {
	const nArts = 4
	q := newTestQueueWithJob(t, "job-1", nArts)
	restoreDone(t, q, "job-1", nArts, 0, 1, 2)

	// The resume read the file and proved only 0 and 2.
	if err := q.ReplaceFromRuns("job-1", []int32{0}, runsOf(0, 2)); err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}

	p := q.SnapshotJob("job-1").Progress()
	for _, i := range []int{0, 2} {
		if !p.ArticleDone(i) {
			t.Errorf("article %d is Outstanding, but the resume proved its bytes are on "+
				"disk; re-fetching it is rework nothing caused", i)
		}
	}
	if p.ArticleDone(1) {
		t.Error("article 1 is still Done after a resume that did not find its bytes — a " +
			"persisted belief outlived the recomputation that disproved it, so the file " +
			"completes with a zero-filled hole (#362)")
	}
	if p.ArticleDone(3) {
		t.Error("article 3 became Done, although neither the restore nor the resume " +
			"claimed it")
	}
}

// TestReplaceFromRuns_CorrectsTheDerivedFigures pins the half a naive
// inverse gets wrong. JobProgress derives failedBytes, byte counts and health
// from the article bits, so clearing a bit without correcting them produces a
// job reporting numbers its own per-article state does not support — which is
// #300 arriving from the other direction.
//
// The reference is a second job that never had the extra bit set at all, so
// the assertion is on agreement with ground truth rather than on figures a
// test author computed by hand.
func TestReplaceFromRuns_CorrectsTheDerivedFigures(t *testing.T) {
	const nArts = 4
	type figures struct {
		fileDownloaded, fileFailed, remaining int64
		filePending, jobPending, resolved     int
	}
	read := func(q *Queue, jobID string) figures {
		snap := q.SnapshotJob(jobID)
		p := snap.Progress()
		return figures{
			fileDownloaded: p.FileBytesDownloaded(0),
			fileFailed:     p.FileFailedBytes(0),
			remaining:      p.RemainingBytes(),
			filePending:    p.FilePending(0),
			jobPending:     p.PendingArticles(),
			resolved:       p.ArticlesResolved(),
		}
	}

	// Reference: 0 and 2 are the only articles that were ever Done.
	ref := newTestQueueWithJob(t, "job-ref", nArts)
	restoreDone(t, ref, "job-ref", nArts, 0, 2)
	want := read(ref, "job-ref")

	// Subject: 0, 1 and 2 restored, then a resume that proves only 0 and 2.
	sub := newTestQueueWithJob(t, "job-sub", nArts)
	restoreDone(t, sub, "job-sub", nArts, 0, 1, 2)
	before := read(sub, "job-sub")
	if before == want {
		t.Fatalf("the extra restored article moved no figure (%+v) — every assertion "+
			"below would hold against a do-nothing implementation", before)
	}

	if err := sub.ReplaceFromRuns("job-sub", []int32{0}, runsOf(0, 2)); err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}
	if got := read(sub, "job-sub"); got != want {
		t.Errorf("figures after the clear = %+v, want %+v (a job that never had the bit "+
			"set) — the bit was cleared without correcting what derives from it, so the "+
			"job reports a health its per-article state does not support", got, want)
	}
}

// TestReplaceFromRuns_HandlesAFileAlreadyComplete pins the rule for a file
// the assembler had finished with.
//
// Complete means "the assembler is finished with this file", NOT "every
// article arrived": a permanently failed article is handed to the assembler,
// which closes the file with a gap. So the rule cannot be "a Complete file
// whose articles are not all durable is stale". It is narrower — the flag is
// dropped only where a bit was actually CLEARED.
//
// Both files are exercised in one job so the two outcomes cannot be asserted
// against different fixtures:
//
//	file 0 — Complete, one article permanently failed, every SUCCESSFUL
//	         article verified. Nothing changes: the failed article's bytes
//	         were never on disk, so their absence is the recorded outcome.
//	file 1 — Complete, every article recorded done, one of them disproved.
//	         The flag and the assembled CRC both go.
func TestReplaceFromRuns_HandlesAFileAlreadyComplete(t *testing.T) {
	const perFile = 3
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "job-1", 2, perFile)
	job.ID = "job-1"
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// File 0: articles 0 and 1 downloaded, article 2 permanently failed.
	seed := append(fileRunsOf(0, 0, 0, 1), fileRunsOf(1, perFile, 0, 1, 2)...)
	if err := q.SeedFromRuns("job-1", seed); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	if err := q.AckPermanentFailure("job-1", []int32{2}); err != nil {
		t.Fatalf("AckPermanentFailure: %v", err)
	}
	for _, fi := range []int{0, 1} {
		if err := q.MarkFileComplete("job-1", fi); err != nil {
			t.Fatalf("MarkFileComplete(%d): %v", fi, err)
		}
		if err := q.SetFileCRC32("job-1", fi, 0xC0FFEE); err != nil {
			t.Fatalf("SetFileCRC32(%d): %v", fi, err)
		}
	}
	pre := q.SnapshotJob("job-1").Progress()
	if !pre.ArticleFailed(2) || !pre.ArticleDone(2) {
		t.Fatalf("file 0's article 2 is not permanently failed (done=%v failed=%v); the "+
			"Complete-with-a-gap case is not what this fixture built",
			pre.ArticleDone(2), pre.ArticleFailed(2))
	}
	if !pre.FileComplete(0) || !pre.FileComplete(1) {
		t.Fatal("a file did not come out of the fixture Complete")
	}

	// The resume finds every successful article of file 0, and only two of
	// file 1's three.
	proved := append(fileRunsOf(0, 0, 0, 1), fileRunsOf(1, perFile, 0, 2)...)
	if err := q.ReplaceFromRuns("job-1", []int32{0, 1}, proved); err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}

	p := q.SnapshotJob("job-1").Progress()
	if !p.FileComplete(0) {
		t.Error("file 0 lost its Complete flag, although every article the resume could " +
			"prove was proved — its only missing article is permanently failed, and a " +
			"restart that un-completes it re-downloads the whole file forever")
	}
	if !p.ArticleDone(2) || !p.ArticleFailed(2) {
		t.Errorf("file 0's permanently failed article came back done=%v failed=%v, want "+
			"true/true — its bytes were never on disk, so their absence is the recorded "+
			"outcome and not new evidence", p.ArticleDone(2), p.ArticleFailed(2))
	}
	if got := p.FileAssembledCRC32(0); got != 0xC0FFEE {
		t.Errorf("file 0's assembled CRC = %#x, want %#x — nothing about the file changed",
			got, 0xC0FFEE)
	}
	if p.FileComplete(1) {
		t.Error("file 1 is still Complete after the resume disproved one of its articles " +
			"— the assembler has bytes left to write into it, so it is not finished (#362)")
	}
	if got := p.FileAssembledCRC32(1); got != 0 {
		t.Errorf("file 1's assembled CRC = %#x, want 0; it describes a whole assembled "+
			"file that has since lost bytes, and QuickCheck would trust it", got)
	}
	if p.ArticleDone(perFile + 1) {
		t.Error("file 1's article 1 is still Done after the resume did not find its bytes")
	}
	for _, i := range []int{perFile + 0, perFile + 2} {
		if !p.ArticleDone(i) {
			t.Errorf("global article %d is Outstanding, although the resume proved it", i)
		}
	}
}

// TestSeedFromRuns_StaysAdditive is the regression guard for the whole of
// #362's fix, and it fails the moment someone "simplifies" the two seeding
// entry points into one.
//
// Application.reevaluateStall's phase 3 replays runs loaded from the store
// after a stall recovery. It is re-delivering an ack whose fsync already
// landed and it has stat'ed nothing, so a clear there would discard the acks
// this process made since the last commit — the exact bits that phase exists
// to preserve.
func TestSeedFromRuns_StaysAdditive(t *testing.T) {
	const nArts = 4
	q := newTestQueueWithJob(t, "job-1", nArts)
	restoreDone(t, q, "job-1", nArts, 1)

	// The stored record covers only article 0. Article 1's bit came from an
	// ack this process made after that commit.
	if err := q.SeedFromRuns("job-1", runsOf(0)); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}

	p := q.SnapshotJob("job-1").Progress()
	if !p.ArticleDone(1) {
		t.Error("article 1 lost its Done bit to a replay of a record that predates it; " +
			"the stall recovery threw away an ack that had already landed")
	}
	if !p.ArticleDone(0) {
		t.Error("article 0 was not marked Done, so the replay installed nothing and the " +
			"additive claim above holds vacuously")
	}
}

// TestMarkNotDone_GuardsWhatItMayUndo pins the inverse of markDone at its own
// level, because two of its three arms are invisible from ReplaceFromRuns'
// return value — it reports nothing, and both refusals leave the article
// exactly as it found it.
//
// The permanently-failed arm is the load-bearing one. failed implies done, so
// a naive "clear whatever is done" inverse would un-fail an article whose
// bytes were never on disk, re-fetch it on every restart and return its bytes
// to the job's health figures as if they might still arrive.
func TestMarkNotDone_GuardsWhatItMayUndo(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 3)
	q.mu.Lock()
	defer q.mu.Unlock()
	job := q.byID["job-1"]
	m, p := job.manifest, job.progress

	// article 0: downloaded. article 1: permanently failed. article 2: never
	// resolved.
	p.markDone(m, 0)
	if !p.markFailed(m, 1) {
		t.Fatal("the fixture's markFailed was a no-op, so article 1 is not in the state " +
			"this test is about")
	}
	if !p.done.Get(1) || !p.failed.Get(1) {
		t.Fatalf("article 1 is done=%v failed=%v, want both true", p.done.Get(1), p.failed.Get(1))
	}

	if !p.markNotDone(0) {
		t.Error("markNotDone refused a plainly downloaded article, so the resume sweep " +
			"can never contradict a stale bit at all")
	}
	if p.done.Get(0) {
		t.Error("article 0 is still Done although markNotDone reported it cleared")
	}
	if p.markNotDone(1) {
		t.Error("markNotDone cleared a permanently failed article; its bytes were never " +
			"on disk, so their absence is the recorded outcome and not new evidence")
	}
	if !p.done.Get(1) || !p.failed.Get(1) {
		t.Errorf("article 1 came out done=%v failed=%v, want both still true",
			p.done.Get(1), p.failed.Get(1))
	}
	if p.markNotDone(2) {
		t.Error("markNotDone reported a change on an article that was never Done; the " +
			"caller counts that return to decide whether the file stops being Complete")
	}
	if p.markNotDone(0) {
		t.Error("markNotDone reported a second change on an article it had already " +
			"cleared, so the count of cleared articles double-counts")
	}
}

// TestRunsCoverage_RefusesARunOutsideItsFile pins the indexing rule both
// seeding entry points depend on and neither can observe.
//
// A Run names articles GLOBALLY, and it also names the file it belongs to. The
// two can disagree only one way: the manifest was rebuilt to a different shape
// under rows keyed on the old numbering. Marking whatever articles happen to
// sit at those indices Done would skip real downloads silently, which is why
// this refuses loudly (A2, R28) rather than clamping or skipping.
//
// The accept case is here too, because a check that refused everything would
// satisfy the refusal arms on its own.
func TestRunsCoverage_RefusesARunOutsideItsFile(t *testing.T) {
	const perFile = 4
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "job-1", 2, perFile)
	job.ID = "job-1"
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m := job.manifest

	t.Run("a run inside its file is accepted with its global indices", func(t *testing.T) {
		// File 1 owns global articles 4..7.
		first, last, err := runsCoverage(m, durability.Run{FileIdx: 1, FirstArtIdx: 5, LastArtIdx: 6})
		if err != nil {
			t.Fatalf("runsCoverage: %v", err)
		}
		if first != 5 || last != 6 {
			t.Errorf("coverage = [%d,%d], want [5,6] — a Run's article indices are already "+
				"global, so there is no conversion to apply", first, last)
		}
	})

	for _, tc := range []struct {
		name string
		run  durability.Run
	}{
		{"a file index the job does not have", durability.Run{FileIdx: 2, FirstArtIdx: 0, LastArtIdx: 0}},
		{"a negative file index", durability.Run{FileIdx: -1, FirstArtIdx: 0, LastArtIdx: 0}},
		{"articles belonging to another file", durability.Run{FileIdx: 1, FirstArtIdx: 0, LastArtIdx: 1}},
		{"a run running past its file's end", durability.Run{FileIdx: 0, FirstArtIdx: 3, LastArtIdx: 4}},
		{"an inverted range", durability.Run{FileIdx: 0, FirstArtIdx: 2, LastArtIdx: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := runsCoverage(m, tc.run); err == nil {
				t.Error("no error; the caller would mark articles Done that this run " +
					"does not describe, and the downloads it skips are silent")
			}
		})
	}
}

// TestAckDurable_ExternallyConstructibleEmptyProofAcksNothing pins the runtime
// half of the proof guarantee.
//
// The compile-time half is narrower than the design's headline. Go permits a
// composite literal with no field values even when every field is unexported,
// so `durability.DurableProof{}` compiles in THIS package — which is outside
// internal/durability, and the fact that this file builds is itself the
// evidence. What the compiler actually bounds is the proof's PAYLOAD: there is
// no exported way to put an article into one, so an externally built proof is
// necessarily empty. AckDurable's `len(arts) == 0` early return is what turns
// "empty" into "inert", and that one line is therefore part of the invariant
// rather than a defensive nicety.
//
// The assertion is deliberately not "nothing is done", which the fixture would
// satisfy on its own before any ack. A real proof for article 0 is applied
// first, so the done-set has a non-trivial value to preserve, and the empty
// proof must leave it EXACTLY there — neither widening it (the over-claim the
// design forbids) nor erroring.
func TestAckDurable_ExternallyConstructibleEmptyProofAcksNothing(t *testing.T) {
	const jobID, nArts = "empty-proof", 4
	q := newTestQueueWithJob(t, jobID, nArts)

	// A genuine barrier-minted ack first, so the state under test is not the
	// zero state.
	if err := q.AckDurable(mintProof(t, jobID, []int32{0})); err != nil {
		t.Fatalf("AckDurable(real proof): %v", err)
	}
	before := outstandingFor(q, jobID)
	if want := []int32{1, 2, 3}; !slices.Equal(before, want) {
		t.Fatalf("outstanding after the real ack = %v, want %v", before, want)
	}

	// The only kind of proof a package outside internal/durability can build.
	// If this line ever fails to compile, the compile-time bound got STRONGER
	// and this test should be deleted, not weakened.
	var external durability.DurableProof
	if got := external.Articles(); len(got) != 0 {
		t.Fatalf("an externally constructed proof named %d articles, want 0; "+
			"the compile-time payload bound is broken", len(got))
	}
	if got := external.JobID(); got != "" {
		t.Fatalf("an externally constructed proof named job %q, want empty", got)
	}

	if err := q.AckDurable(external); err != nil {
		t.Fatalf("AckDurable(empty proof) = %v, want nil (an empty proof is a no-op, "+
			"not an error and not a job-wide ack)", err)
	}

	after := outstandingFor(q, jobID)
	if !slices.Equal(after, before) {
		t.Errorf("outstanding changed across an empty-proof ack: %v -> %v; "+
			"an empty proof must resolve nothing", before, after)
	}
}

// Confirm records the release so a test can tell a confirmed cycle from an
// abandoned one; the real writer drops its drain report here.
func (s *stubSyncTarget) Confirm(_ context.Context, idx int32) {
	s.confirmed = append(s.confirmed, idx)
}
