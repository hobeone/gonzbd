package queue

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// stubSyncTarget is a durability.SyncTarget with a fixed answer. It reports
// one file whose articles are already written, so a barrier run over it does
// no I/O and reaches the ack.
type stubSyncTarget struct {
	files    []int32
	written  []durability.WrittenArticle
	artCount int
}

func (s *stubSyncTarget) Files() []int32 { return s.files }
func (s *stubSyncTarget) Drain(context.Context, int32) ([]durability.WrittenArticle, error) {
	return s.written, nil
}
func (s *stubSyncTarget) Sync(context.Context, int32) error { return nil }
func (s *stubSyncTarget) Stat(int32) (int64, int64, error)  { return 4096, 1, nil }
func (s *stubSyncTarget) ArticleCount(int32) int            { return s.artCount }
func (s *stubSyncTarget) FileLocalOrdinal(_ int32, a int32) (int, bool) {
	// The stub's single file starts at global article 0, so the global index
	// is the file-local ordinal. Anything past the file's article count is
	// rejected, which is what the barrier treats as a bookkeeping defect.
	if int(a) >= s.artCount {
		return 0, false
	}
	return int(a), true
}

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
// and that absence is what makes "ack only after fsync" compiler-enforced
// rather than a rule six call sites must each remember. A test-only exported
// constructor would move that guarantee from the compiler to a CI grep, so
// this helper pays the setup cost instead — and gets a test that exercises the
// real minting path as a bonus.
func mintProof(t *testing.T, jobID string, arts []int32, artCount int) durability.DurableProof {
	t.Helper()

	written := make([]durability.WrittenArticle, len(arts))
	for i, a := range arts {
		written[i] = durability.WrittenArticle{
			FileIdx: 0, ArtIdx: a, Offset: int64(a) * 100, Length: 100,
		}
	}
	tgt := &stubSyncTarget{files: []int32{0}, written: written, artCount: artCount}

	var got durability.DurableProof
	captured := ackerFunc(func(p durability.DurableProof) error { got = p; return nil })

	hdb, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "mint.db"))
	if err != nil {
		t.Fatalf("mintProof: open db: %v", err)
	}
	t.Cleanup(func() { _ = hdb.Close() })
	db := history.NewRepository(hdb).DB()

	b := durability.NewBarrier(
		durability.NewSQLiteFactLog(db),
		durability.NewSQLiteExtentStore(db),
		captured,
		noopStallable{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := b.Run(context.Background(), jobID, tgt); err != nil {
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
	p := mintProof(t, "job-1", []int32{0, 3, 7}, 10)

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
	p := mintProof(t, "job-1", []int32{0, 1, 2}, 10)
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
	p := mintProof(t, "job-1", []int32{0, 6}, 8)

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
