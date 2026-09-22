package app

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// commitRuns records arts in durable_runs for jobID and returns the collisions
// the commit reported.
//
// It goes through a real durability.Barrier because nothing else outside
// internal/durability can write run content: Store.commit is unexported, and
// the barrier is its one production caller. A fixture that wrote rows with SQL instead
// would have to re-implement the commit's dedup, merge and CRC combine, and a
// test built on that copy would pass against the copy.
func commitRuns(t *testing.T, st *durability.Store, jobID string, arts []durability.DurableArticle) []durability.Collision {
	t.Helper()

	var cols []durability.Collision
	capture := durability.WithCommitWrap(func(_ context.Context, _ string, commit func() ([]durability.Collision, error)) ([]durability.Collision, error) {
		c, err := commit()
		cols = append(cols, c...)
		return c, err
	})
	b := durability.NewBarrier(st, discardAcker{}, discardStallable{}, slog.New(slog.DiscardHandler), capture)
	if _, err := b.Run(context.Background(), jobID, newWrittenTarget(arts)); err != nil {
		t.Fatalf("commitRuns: barrier run: %v", err)
	}
	return cols
}

// realStore is the application's *durability.Store, for a test that builds its
// own barrier over it. It fails the test if a double has replaced the store.
func realStore(t *testing.T, application *Application) *durability.Store {
	t.Helper()
	st, ok := application.durable.(*durability.Store)
	if !ok {
		t.Fatalf("want the application's real *durability.Store, got %T", application.durable)
	}
	return st
}

// writtenTarget is a durability.SyncTarget reporting a fixed set of articles
// as written and synced, sized so no stat check finds a shortfall.
type writtenTarget struct {
	byFile map[int32][]durability.WrittenArticle
	size   map[int32]int64
}

func newWrittenTarget(arts []durability.DurableArticle) *writtenTarget {
	w := &writtenTarget{byFile: map[int32][]durability.WrittenArticle{}, size: map[int32]int64{}}
	for _, a := range arts {
		w.byFile[a.FileIdx] = append(w.byFile[a.FileIdx], durability.WrittenArticle(a))
		w.size[a.FileIdx] = max(w.size[a.FileIdx], a.Offset+int64(a.Length))
	}
	return w
}

func (w *writtenTarget) Files() []int32 {
	return slices.Sorted(maps.Keys(w.byFile))
}

func (w *writtenTarget) Path(int32) string { return "/downloads/fixture.bin" }

func (w *writtenTarget) Drain(_ context.Context, fi int32) ([]durability.WrittenArticle, error) {
	return w.byFile[fi], nil
}

func (w *writtenTarget) Sync(context.Context, int32) error    { return nil }
func (w *writtenTarget) Confirm(context.Context, int32)       {}
func (w *writtenTarget) Stat(fi int32) (int64, error)         { return w.size[fi], nil }
func (discardAcker) AckDurable(durability.DurableProof) error { return nil }
func (discardStallable) Stall(string, *storagefault.Fault)    {}
func (discardStallable) Fail(string, *storagefault.Fault)     {}

// discardAcker and discardStallable accept whatever the fixture's barrier
// reports; commitRuns wants the rows, not the acks.
type (
	discardAcker     struct{}
	discardStallable struct{}
)
