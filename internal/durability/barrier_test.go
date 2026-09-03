package durability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"syscall"

	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.DiscardHandler)
}

// fakeTarget records the order of Drain/Sync/Stat calls so the ordering
// invariant S1 can be asserted rather than assumed.
type fakeTarget struct {
	confirmed []int32
	calls     []string
	files     []int32
	written   map[int32][]WrittenArticle
	drainErr  error
	syncErr   error
	statErr   error
	size      int64
}

func (f *fakeTarget) Files() []int32 {
	if f.files != nil {
		return f.files
	}
	return []int32{0}
}

// Path is the R27 stall-reason accessor. The fixture returns a fixed name so
// the fault-routing tests can assert the fault carries it rather than "".
func (f *fakeTarget) Path(fileIdx int32) string {
	return fmt.Sprintf("/downloads/job-1/file%d.bin", fileIdx)
}

func (f *fakeTarget) Drain(_ context.Context, fileIdx int32) ([]WrittenArticle, error) {
	f.calls = append(f.calls, "drain")
	if f.drainErr != nil {
		return nil, f.drainErr
	}
	return f.written[fileIdx], nil
}

func (f *fakeTarget) Sync(_ context.Context, _ int32) error {
	f.calls = append(f.calls, "sync")
	return f.syncErr
}

func (f *fakeTarget) Stat(_ int32) (int64, error) {
	f.calls = append(f.calls, "stat")
	if f.statErr != nil {
		return 0, f.statErr
	}
	return f.size, nil
}

// Confirm records the release so a test can tell a confirmed cycle from an
// unconfirmed one.
func (f *fakeTarget) Confirm(_ context.Context, idx int32) { f.confirmed = append(f.confirmed, idx) }

var _ SyncTarget = (*fakeTarget)(nil)

type recordingAcker struct {
	proofs []DurableProof
	err    error
}

func (r *recordingAcker) AckDurable(p DurableProof) error {
	r.proofs = append(r.proofs, p)
	return r.err
}

type recordingStall struct {
	stalled []*storagefault.Fault
	failed  []*storagefault.Fault
}

func (r *recordingStall) Stall(_ string, f *storagefault.Fault) { r.stalled = append(r.stalled, f) }
func (r *recordingStall) Fail(_ string, f *storagefault.Fault)  { r.failed = append(r.failed, f) }

// newBarrierWithStore is the common wiring for the tests that need no error
// injection in the store itself.
func newBarrierWithStore(t *testing.T, rs RunStore, ack Acker, stall Stallable) *Barrier {
	t.Helper()
	return NewBarrier(rs, ack, stall, testLogger(t))
}

// TestBarrier_SyncPrecedesCommitAndAck is the pin for S1 and R9. If the
// commit or the ack happens before the fsync, this fails.
func TestBarrier_SyncPrecedesCommitAndAck(t *testing.T) {
	ctx := context.Background()
	tgt := &fakeTarget{
		written: map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
		size:    100,
	}
	ack := &recordingAcker{}
	b := newBarrierWithStore(t, NewSQLiteRunStore(openTestDB(t)), ack, &recordingStall{})

	if _, err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	want := []string{"drain", "sync", "stat"}
	if len(tgt.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", tgt.calls, want)
	}
	for i := range want {
		if tgt.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v — S1 requires sync before any claim", tgt.calls, want)
		}
	}
	if len(ack.proofs) != 1 || len(ack.proofs[0].Articles()) != 1 {
		t.Fatalf("expected exactly one article acked, got %v", ack.proofs)
	}
	if ack.proofs[0].JobID() != "job-1" {
		t.Errorf("proof job = %q, want job-1", ack.proofs[0].JobID())
	}
}

// TestBarrier_SyncFailureAcksNothing pins R7: a failed barrier acks nothing
// and leaves the stored runs intact.
func TestBarrier_SyncFailureAcksNothing(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	if _, err := rs.Commit(ctx, "job-1", []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1111},
	}); err != nil {
		t.Fatal(err)
	}

	tgt := &fakeTarget{
		written: map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100}}},
		syncErr: syscall.ENOSPC,
	}
	ack := &recordingAcker{}
	stall := &recordingStall{}
	b := newBarrierWithStore(t, rs, ack, stall)

	if _, err := b.Run(ctx, "job-1", tgt); err == nil {
		t.Fatal("Run returned nil after a failed sync")
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked %d proofs after a failed sync, want 0", len(ack.proofs))
	}
	got, err := rs.ForJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded %d runs, want 1", len(got))
	}
	if got[0].LastArtIdx != 0 || got[0].Length != 100 {
		t.Errorf("the stored run was disturbed by a failed barrier: %+v", got[0])
	}
	if len(stall.stalled) != 1 {
		t.Fatalf("stalled %d times, want 1 — ENOSPC is retryable", len(stall.stalled))
	}
	if got := stall.stalled[0].Path; got != "/downloads/job-1/file0.bin" {
		t.Errorf("stall fault Path = %q, want the target's path (R27)", got)
	}
	if len(stall.failed) != 0 {
		t.Errorf("failed the job on a retryable fault")
	}
}

// TestBarrier_PermanentFaultFailsRatherThanStalls pins R20.
func TestBarrier_PermanentFaultFailsRatherThanStalls(t *testing.T) {
	ctx := context.Background()
	tgt := &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, syncErr: syscall.EROFS}
	stall := &recordingStall{}
	b := newBarrierWithStore(t, NewSQLiteRunStore(openTestDB(t)), &recordingAcker{}, stall)

	if _, err := b.Run(ctx, "job-1", tgt); err == nil {
		t.Fatal("Run returned nil after EROFS")
	}
	if len(stall.failed) != 1 {
		t.Errorf("failed %d times, want 1 — EROFS is permanent", len(stall.failed))
	}
	if len(stall.stalled) != 0 {
		t.Errorf("stalled on a permanent fault")
	}
}

// TestBarrier_DrainAndStatFaultsRouteThroughA1 pins that the two phases
// either side of the fsync classify their own failures rather than
// returning a bare error, and that neither marks an article failed (A1).
func TestBarrier_DrainAndStatFaultsRouteThroughA1(t *testing.T) {
	tests := []struct {
		name    string
		tgt     *fakeTarget
		wantCal []string
	}{
		{
			name:    "drain fault stops before any sync",
			tgt:     &fakeTarget{drainErr: syscall.ENOSPC},
			wantCal: []string{"drain"},
		},
		{
			name:    "stat fault stops after the sync",
			tgt:     &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, statErr: syscall.ENOSPC},
			wantCal: []string{"drain", "sync", "stat"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			stall := &recordingStall{}
			ack := &recordingAcker{}
			b := newBarrierWithStore(t, NewSQLiteRunStore(openTestDB(t)), ack, stall)

			_, err := b.Run(ctx, "job-1", tt.tgt)
			if err == nil {
				t.Fatal("Run returned nil after a storage fault")
			}
			if !errors.Is(err, syscall.ENOSPC) {
				t.Errorf("err = %v, want it to wrap ENOSPC", err)
			}
			if len(stall.stalled) != 1 {
				t.Fatalf("stalled %d times, want 1", len(stall.stalled))
			}
			// R27: a stall reason has to be actionable, and "ENOSPC on
			// sync" without a path does not say which volume filled up.
			// A job's files can live on different mounts, so the job name
			// does not identify the device either.
			if got := stall.stalled[0].Path; got != "/downloads/job-1/file0.bin" {
				t.Errorf("stall fault Path = %q, want the target's path — "+
					"the user is told a disk is full without being told which one", got)
			}
			if len(ack.proofs) != 0 {
				t.Errorf("acked after a storage fault")
			}
			if len(tt.tgt.calls) != len(tt.wantCal) {
				t.Fatalf("calls = %v, want %v", tt.tgt.calls, tt.wantCal)
			}
			for i := range tt.wantCal {
				if tt.tgt.calls[i] != tt.wantCal[i] {
					t.Fatalf("calls = %v, want %v", tt.tgt.calls, tt.wantCal)
				}
			}
		})
	}
}

// errStore fails Commit so the ack-after-commit ordering can be pinned from
// the commit side as well as the sync side.
type errStore struct {
	RunStore
	err error
}

func (e *errStore) Commit(context.Context, string, []DurableArticle) ([]Collision, error) {
	return nil, e.err
}

// TestBarrier_CommitFailureAcksNothing pins the second half of phase 4: the
// ack is downstream of the commit, so a commit that fails must ack nothing.
func TestBarrier_CommitFailureAcksNothing(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("commit exploded")
	rs := &errStore{RunStore: NewSQLiteRunStore(openTestDB(t)), err: boom}
	tgt := &fakeTarget{
		written: map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
	}
	ack := &recordingAcker{}
	b := newBarrierWithStore(t, rs, ack, &recordingStall{})

	_, err := b.Run(ctx, "job-1", tgt)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the commit failure", err)
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked %d proofs after a failed commit, want 0", len(ack.proofs))
	}
}

// TestBarrier_NothingDrainedAcksNothing pins that an idle barrier emits no
// proof and records nothing — an empty proof would be an ack of nothing and
// pointless work for the queue, and an empty commit has no article to place.
//
// It still CONFIRMS, and that half matters more than it looks: a job whose
// files are all already durable drains an empty report on every checkpoint,
// and a cycle that landed without confirming would re-report it forever.
func TestBarrier_NothingDrainedAcksNothing(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	tgt := &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, size: 7}
	ack := &recordingAcker{}
	b := newBarrierWithStore(t, rs, ack, &recordingStall{})

	if _, err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked %d proofs with nothing drained, want 0", len(ack.proofs))
	}
	got, err := rs.ForJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("recorded %d runs with nothing drained, want 0: %+v", len(got), got)
	}
	if len(tgt.confirmed) != 1 {
		t.Errorf("confirmed %v, want file 0 — an unconfirmed idle cycle re-reports forever",
			tgt.confirmed)
	}
}

// TestBarrier_AckFailurePropagates pins that a rejected ack is returned
// rather than swallowed (A2).
func TestBarrier_AckFailurePropagates(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("queue rejected the proof")
	ack := &recordingAcker{err: boom}
	tgt := &fakeTarget{
		written: map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
	}
	b := newBarrierWithStore(t, NewSQLiteRunStore(openTestDB(t)), ack, &recordingStall{})

	if _, err := b.Run(ctx, "job-1", tgt); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the ack failure", err)
	}
}

// TestBarrier_MultipleFilesSyncAllBeforeAnyClaim pins that every file is
// drained and fsynced before ANY of them is claimed, so a barrier that fails
// on the second file's sync has claimed nothing about the first either.
func TestBarrier_MultipleFilesSyncAllBeforeAnyClaim(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	tgt := &fakeTarget{
		files: []int32{0, 1},
		written: map[int32][]WrittenArticle{
			0: {{FileIdx: 0, ArtIdx: 5, Offset: 0, Length: 10}},
			1: {{FileIdx: 1, ArtIdx: 1, Offset: 0, Length: 20}},
		},
		size: 1 << 20,
	}
	ack := &recordingAcker{}
	b := newBarrierWithStore(t, rs, ack, &recordingStall{})
	if _, err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	want := []string{"drain", "drain", "sync", "sync", "stat", "stat"}
	if len(tgt.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", tgt.calls, want)
	}
	for i := range want {
		if tgt.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v — every sync precedes every claim", tgt.calls, want)
		}
	}
	if len(ack.proofs) != 1 {
		t.Fatalf("acked %d proofs, want 1 covering both files", len(ack.proofs))
	}
	arts := ack.proofs[0].Articles()
	if len(arts) != 2 || arts[0] != 1 || arts[1] != 5 {
		t.Errorf("proof articles = %v, want [1 5] — drained as 5 then 1, so the proof is sorted", arts)
	}

	got, err := rs.ForJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("recorded %d runs, want one per file", len(got))
	}
	// ForJob orders by file_idx then offset, so index i is file i here.
	for i, wantLen := range []int64{10, 20} {
		if got[i].FileIdx != int32(i) {
			t.Errorf("run %d has FileIdx %d, want %d", i, got[i].FileIdx, i)
		}
		if got[i].Length != wantLen {
			t.Errorf("file %d run Length = %d, want %d", i, got[i].Length, wantLen)
		}
	}
	if got[0].FirstArtIdx != 5 || got[0].LastArtIdx != 5 {
		t.Errorf("file 0's run covers [%d,%d], want [5,5]", got[0].FirstArtIdx, got[0].LastArtIdx)
	}
	if got[1].FirstArtIdx != 1 || got[1].LastArtIdx != 1 {
		t.Errorf("file 1's run covers [%d,%d], want [1,1]", got[1].FirstArtIdx, got[1].LastArtIdx)
	}
}

// TestBarrier_ConfirmsOnlyAfterTheCommitAndAck pins where the drain report is
// released: below both, never on the fsync.
func TestBarrier_ConfirmsOnlyAfterTheCommitAndAck(t *testing.T) {
	ctx := context.Background()
	drain := map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}}

	t.Run("a successful cycle confirms", func(t *testing.T) {
		b := newBarrierWithStore(t, NewSQLiteRunStore(openTestDB(t)),
			&recordingAcker{}, &recordingStall{})
		tgt := &fakeTarget{written: drain, size: 100}
		if _, err := b.Run(ctx, "job-1", tgt); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(tgt.confirmed) == 0 {
			t.Error("a cycle that committed and acked never confirmed; the report is " +
				"retained forever and every later checkpoint re-reports the whole file")
		}
	})

	t.Run("a failed ack does not confirm", func(t *testing.T) {
		b := newBarrierWithStore(t, NewSQLiteRunStore(openTestDB(t)),
			&recordingAcker{err: errors.New("job not resident")}, &recordingStall{})
		tgt := &fakeTarget{written: drain, size: 100}
		if _, err := b.Run(ctx, "job-1", tgt); err == nil {
			t.Fatal("Run reported success while the ack failed")
		}
		if len(tgt.confirmed) != 0 {
			t.Errorf("the report was confirmed for file %v despite the ack failing. "+
				"The retry drains nothing, so those articles are never acked and the "+
				"file cannot complete for the life of the handle", tgt.confirmed)
		}
	})
}

// TestConfirmAll_ReleasesEveryFileOfTheJob pins that the release covers the
// whole job rather than the file that happened to be last.
//
// Run drains and syncs every open file before it claims anything, so a cycle
// that lands has made claims about all of them. Confirming only some would
// leave the rest re-reporting forever.
func TestConfirmAll_ReleasesEveryFileOfTheJob(t *testing.T) {
	b := newBarrierWithStore(t, NewSQLiteRunStore(openTestDB(t)),
		&recordingAcker{}, &recordingStall{})
	tgt := &fakeTarget{}

	b.confirmAll(context.Background(), []int32{0, 1, 2}, tgt)

	if len(tgt.confirmed) != 3 {
		t.Fatalf("confirmed %v, want all three files", tgt.confirmed)
	}
	for i, idx := range []int32{0, 1, 2} {
		if tgt.confirmed[i] != idx {
			t.Errorf("confirmed[%d] = %d, want %d", i, tgt.confirmed[i], idx)
		}
	}
}

// TestRouteFault pins A1's dispatch table and that the fault is also
// returned, so a caller that ignores Stallable still cannot read a storage
// fault as success.
func TestRouteFault(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStalled int
		wantFailed  int
	}{
		{"retryable stalls", syscall.ENOSPC, 1, 0},
		{"permanent fails", syscall.EROFS, 0, 1},
		{"unrecognised defaults to retryable", errors.New("weird"), 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stall := &recordingStall{}
			b := NewBarrier(nil, nil, stall, testLogger(t))
			f := storagefault.Classify("sync", "/x", tt.err)

			got := b.routeFault("job-1", f)
			if !errors.Is(got, tt.err) {
				t.Errorf("routeFault returned %v, want it to wrap %v", got, tt.err)
			}
			if !errors.Is(got, ErrFaultRouted) {
				t.Errorf("routeFault returned %v, want it to carry ErrFaultRouted so the "+
					"caller does not park the job a second time", got)
			}
			if len(stall.stalled) != tt.wantStalled {
				t.Errorf("stalled %d, want %d", len(stall.stalled), tt.wantStalled)
			}
			if len(stall.failed) != tt.wantFailed {
				t.Errorf("failed %d, want %d", len(stall.failed), tt.wantFailed)
			}
		})
	}
}

func TestNewTestDurableProof(t *testing.T) {
	t.Parallel()
	proof := NewTestDurableProof("job-123", []int32{1, 2, 3})
	if proof.JobID() != "job-123" {
		t.Errorf("JobID = %q, want job-123", proof.JobID())
	}
	if len(proof.Articles()) != 3 {
		t.Errorf("Articles len = %d, want 3", len(proof.Articles()))
	}
}
