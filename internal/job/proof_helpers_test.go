package job

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
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
func (s *stubSyncTarget) Confirm(_ context.Context, fileIdx int32) {
	s.confirmed = append(s.confirmed, fileIdx)
}
func (s *stubSyncTarget) Stat(int32) (int64, error) { return 4096, nil }

// ackerFunc is a func-typed durability.Acker.
type ackerFunc func(durability.DurableProof) error

func (f ackerFunc) AckDurable(p durability.DurableProof) error { return f(p) }

// noopStallable satisfies durability.Stallable. A barrier run in these tests
// never faults, so neither method should fire.
type noopStallable struct{}

func (noopStallable) Stall(string, *storagefault.Fault) {}
func (noopStallable) Fail(string, *storagefault.Fault)  {}

// stubRunStore satisfies durability.RunStore without needing SQLite or disk I/O.
type stubRunStore struct{}

func (stubRunStore) Commit(context.Context, string, []durability.DurableArticle) ([]durability.Collision, error) {
	return nil, nil
}
func (stubRunStore) ForFile(context.Context, string, int32) ([]durability.Run, error) {
	return nil, nil
}
func (stubRunStore) ForJob(context.Context, string) ([]durability.Run, error) {
	return nil, nil
}
func (stubRunStore) DeleteFile(context.Context, string, int32) error {
	return nil
}
func (stubRunStore) DeleteJob(context.Context, string) error {
	return nil
}

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

	b := durability.NewBarrier(
		stubRunStore{},
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
