package app

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// modelStore applies batches the way ApplyRecord does (verdict deletes, row
// upserts, file states, verdict complete changes) to an in-memory model. When
// gated, its first call announces itself on entered, then waits for release
// and fails if failFirst.
type modelStore struct {
	mu        sync.Mutex
	rows      map[[2]int]bool
	complete  map[int]bool
	calls     int
	gated     bool
	failFirst bool
	entered   chan struct{}
	release   chan struct{}
}

func newModelStore(gated, failFirst bool) *modelStore {
	return &modelStore{
		rows: map[[2]int]bool{}, complete: map[int]bool{},
		gated: gated, failFirst: failFirst,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
}

func (s *modelStore) ApplyRecord(_ context.Context, batches []durability.RecordBatch) error {
	s.mu.Lock()
	s.calls++
	first := s.gated && s.calls == 1
	s.mu.Unlock()
	if first {
		close(s.entered)
		<-s.release
		if s.failFirst {
			return errors.New("boom")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range batches {
		for _, v := range b.Verdicts {
			for k := range s.rows {
				if k[0] != v.FileIdx {
					continue
				}
				del := v.DeleteAll
				for _, a := range v.DeleteArtIdxs {
					del = del || int(a) == k[1]
				}
				if del {
					delete(s.rows, k)
				}
			}
		}
		for _, r := range b.Rows {
			s.rows[[2]int{r.FileIdx, int(r.ArtIdx)}] = true
		}
		for _, f := range b.Files {
			s.complete[f.FileIdx] = f.Complete
		}
		for _, v := range b.Verdicts {
			if v.SetComplete {
				s.complete[v.FileIdx] = true
			}
			if v.ClearComplete {
				s.complete[v.FileIdx] = false
			}
		}
	}
	return nil
}

// untrustDuringFlush blocks a flush inside the store, runs an untrust of the
// same file, then releases the flush, and returns the final modelled state.
// apply waits on the recorder's writer lock, so the untrust is given a bounded
// time to commit while the flush is blocked: correct code times out and
// proceeds, a recorder that lets apply overtake commits it first. The timeout
// can only let a mutant live (apply too slow to overtake within it), never fail
// correct code, which releases the flush either way.
func untrustDuringFlush(t *testing.T, flushFails bool) *modelStore {
	t.Helper()
	st := newModelStore(true, flushFails)
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, nil, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Length: 10})
	r.markDirty(j, 0, durability.FileState{Complete: true})

	flushErr := make(chan error, 1)
	go func() { flushErr <- r.flush(context.Background()) }()
	<-st.entered

	applyDone := make(chan error, 1)
	go func() {
		applyDone <- r.apply(context.Background(), j, []durability.FileVerdict{{FileIdx: 0, DeleteAll: true, ClearComplete: true}})
	}()
	var applyErr error
	applied := false
	select {
	case e := <-applyDone:
		applyErr, applied = e, true
	case <-time.After(time.Second):
	}
	close(st.release)
	if err := <-flushErr; (err != nil) != flushFails {
		t.Fatalf("flush err = %v, want failure = %v", err, flushFails)
	}
	if !applied {
		applyErr = <-applyDone
	}
	if applyErr != nil {
		t.Fatal(applyErr)
	}
	if flushFails {
		if err := r.flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func checkUntrusted(t *testing.T, st *modelStore) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.rows) != 0 {
		t.Errorf("rows of an untrusted file survived: %v", st.rows)
	}
	if st.complete[0] {
		t.Error("complete=1 survived the untrust of file 0")
	}
}

func TestRecorder_UntrustAfterInFlightSuccessfulFlushWins(t *testing.T) {
	checkUntrusted(t, untrustDuringFlush(t, false))
}

func TestRecorder_UntrustAfterInFlightFailedFlushWins(t *testing.T) {
	checkUntrusted(t, untrustDuringFlush(t, true))
}

// modelRows is the model's rows as (file, art) pairs, sorted.
func modelRows(st *modelStore) [][2]int {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := slices.Collect(maps.Keys(st.rows))
	slices.SortFunc(out, func(a, b [2]int) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
	return out
}

// untrustTimedOut runs an untrust of file 0 of j that cannot take wmu within
// its ctx, and requires that it reported its ctx's deadline.
func untrustTimedOut(t *testing.T, r *recorder, j *job.Job) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.untrust(ctx, j, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("untrust = %v, want its ctx's DeadlineExceeded", err)
	}
}

// untrustOverrunsFlush is the lock-timeout half of #827: a flush holds wmu
// inside the store with file 0's row in its snapshot, an untrust of file 0
// gives up on wmu, and the flush then succeeds or fails. The untrust must
// still purge what is buffered for the file and owe the delete, and the next
// flush must carry the delete ahead of the row refetched after the untrust —
// on a failed flush, after a remerge that did not put the overrun rows back.
func untrustOverrunsFlush(t *testing.T, flushFails bool) (*modelStore, *recorder, *job.Job) {
	t.Helper()
	st := newModelStore(true, flushFails)
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, nil, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	r.noteWritten(j, durability.WrittenRow{FileIdx: 1, ArtIdx: 0, Length: 10})
	// In the snapshot too: a failed flush must not put complete=1 back for
	// the file the untrust overran.
	r.markDirty(j, 0, durability.FileState{FileIdx: 0, Complete: true})

	flushErr := make(chan error, 1)
	go func() { flushErr <- r.flush(context.Background()) }()
	<-st.entered
	// Noted after the snapshot and before the untrust: stale, and still buffered.
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Length: 10})
	r.markDirty(j, 0, durability.FileState{FileIdx: 0, Complete: true, Filename: "later"})

	untrustTimedOut(t, r, j)
	r.mu.Lock()
	pending := slices.Clone(r.pending[j])
	_, dirty := r.dirty[j][0]
	owed := r.owed[j][0]
	r.mu.Unlock()
	if len(pending) != 0 || dirty || owed == 0 {
		t.Fatalf("after the timed-out untrust: pending %v, file 0 dirty %v, owed %d; want the file's buffer purged and the delete owed", pending, dirty, owed)
	}

	// The refetch lands while the flush is still inside the store.
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 3, Length: 10})
	close(st.release)
	if err := <-flushErr; (err != nil) != flushFails {
		t.Fatalf("flush err = %v, want failure = %v", err, flushFails)
	}
	return st, r, j
}

func TestRecorder_UntrustThatOverransASuccessfulFlushIsPaidByTheNext(t *testing.T) {
	st, r, j := untrustOverrunsFlush(t, false)
	st.mu.Lock()
	wrote := st.complete[0]
	st.mu.Unlock()
	if got := modelRows(st); !slices.Equal(got, [][2]int{{0, 1}, {1, 0}}) || !wrote {
		t.Fatalf("fixture: after the flush the untrust overran: rows %v, complete %v; want file 0's stale row and complete=1, and file 1's row", got, wrote)
	}
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := modelRows(st); !slices.Equal(got, [][2]int{{0, 3}, {1, 0}}) {
		t.Errorf("rows after the next flush = %v, want only the refetched row of file 0", got)
	}
	st.mu.Lock()
	complete := st.complete[0]
	st.mu.Unlock()
	if complete {
		t.Error("complete=1 survived the owed delete")
	}
	if got := owedOf(r, j); len(got) != 0 {
		t.Errorf("owed = %v after the flush that carried it, want none", got)
	}
}

func TestRecorder_RemergePutsBackNothingAnUntrustOverran(t *testing.T) {
	st, r, j := untrustOverrunsFlush(t, true)
	r.mu.Lock()
	pending := slices.Clone(r.pending[j])
	_, dirty := r.dirty[j][0]
	r.mu.Unlock()
	if len(pending) != 2 || pending[0].FileIdx != 1 || pending[1] != (durability.WrittenRow{FileIdx: 0, ArtIdx: 3, Length: 10}) || dirty {
		t.Fatalf("after the remerge: pending %v, file 0 dirty %v; want file 1's row back, the refetched row kept, and nothing of file 0 from the snapshot", pending, dirty)
	}
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := modelRows(st); !slices.Equal(got, [][2]int{{0, 3}, {1, 0}}) {
		t.Errorf("rows after the retried flush = %v, want only the refetched row of file 0", got)
	}
	st.mu.Lock()
	complete := st.complete[0]
	st.mu.Unlock()
	if complete {
		t.Error("complete=1 reached the store for the untrusted file: the remerge put the snapshot's file state back")
	}
}

// TestRecorder_RemergePutsBackOnlyWhatNoUntrustOverran calls remerge directly
// with rows and file states for three files: file 0's debt is newer than the
// one the failed write carried, file 1's is the one it carried, file 2 owes
// nothing. Only file 0's rows and state stay out.
func TestRecorder_RemergePutsBackOnlyWhatNoUntrustOverran(t *testing.T) {
	j := newTestJob(t, "id")
	r := newRecorder(&fakeRecordStore{}, func(string) *job.Job { return j }, nil, slog.Default())
	r.owed[j] = map[int]uint64{0: 2, 1: 1}
	r.pending[j] = []durability.WrittenRow{{FileIdx: 0, ArtIdx: 9}}
	rows := map[*job.Job][]durability.WrittenRow{j: {{FileIdx: 0, ArtIdx: 1}, {FileIdx: 1, ArtIdx: 1}, {FileIdx: 2, ArtIdx: 1}}}
	files := map[*job.Job]map[int]durability.FileState{j: {
		0: {FileIdx: 0, Complete: true}, 1: {FileIdx: 1, Complete: true}, 2: {FileIdx: 2, Complete: true},
	}}
	r.remerge(rows, files, map[*job.Job]map[int]uint64{j: {0: 1, 1: 1}})

	want := []durability.WrittenRow{{FileIdx: 1, ArtIdx: 1}, {FileIdx: 2, ArtIdx: 1}, {FileIdx: 0, ArtIdx: 9}}
	if !slices.Equal(r.pending[j], want) {
		t.Errorf("pending = %v, want %v: the overrun file's snapshot rows left out, ahead of the row noted since", r.pending[j], want)
	}
	if _, ok := r.dirty[j][0]; ok {
		t.Error("the overrun file's snapshot state was put back")
	}
	if len(r.dirty[j]) != 2 || !r.dirty[j][1].Complete || !r.dirty[j][2].Complete {
		t.Errorf("dirty = %v, want files 1 and 2 restored", r.dirty[j])
	}
}

// TestRecorder_ASecondUntrustDuringTheFlushKeepsItsDebt pins the sequence
// number: a flush carrying file 0's owed delete writes a row refetched after
// that untrust; a second untrust of the file during the write disowns that
// row; the flush's success settles only the debt it carried, so the next
// flush deletes the row.
func TestRecorder_ASecondUntrustDuringTheFlushKeepsItsDebt(t *testing.T) {
	st := newModelStore(true, false)
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, nil, slog.Default())
	if err := r.lockWriter(context.Background()); err != nil {
		t.Fatal(err)
	}
	untrustTimedOut(t, r, j)
	r.unlockWriter()
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Length: 10})

	flushErr := make(chan error, 1)
	go func() { flushErr <- r.flush(context.Background()) }()
	<-st.entered
	untrustTimedOut(t, r, j)
	close(st.release)
	if err := <-flushErr; err != nil {
		t.Fatal(err)
	}
	if got := modelRows(st); !slices.Equal(got, [][2]int{{0, 2}}) {
		t.Fatalf("fixture: rows after the flush = %v, want the row the second untrust disowned", got)
	}
	if got := owedOf(r, j); len(got) != 1 {
		t.Fatalf("owed = %v after the flush, want the second untrust's debt kept", got)
	}
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 3, Length: 10})
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := modelRows(st); !slices.Equal(got, [][2]int{{0, 3}}) {
		t.Errorf("rows after the next flush = %v, want the disowned row gone", got)
	}
}

// TestRecorder_CompleteNeverPrecedesItsRowUnderConcurrency has a writer note
// file i's row and then mark it complete, without pause, across a fixed number
// of flushes, and requires every flushed complete flag to be accompanied, in
// that flush or an earlier one, by its row.
func TestRecorder_CompleteNeverPrecedesItsRowUnderConcurrency(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, nil, slog.Default())

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r.noteWritten(j, durability.WrittenRow{FileIdx: i, ArtIdx: 0, Length: 1})
			r.markDirty(j, i, durability.FileState{Complete: true})
		}
	}()
	for range 20 {
		_ = r.flush(context.Background())
	}
	close(stop)
	<-done
	_ = r.flush(context.Background())

	rowSeen := make(map[int]bool)
	for _, b := range st.snapshot() {
		for _, row := range b.Rows {
			rowSeen[row.FileIdx] = true
		}
		for _, f := range b.Files {
			if f.Complete && !rowSeen[f.FileIdx] {
				t.Fatalf("file %d complete flushed before its row", f.FileIdx)
			}
		}
	}
}

func TestRecorder_PurgeLocked(t *testing.T) {
	rows := func() []durability.WrittenRow {
		return []durability.WrittenRow{
			{FileIdx: 0, ArtIdx: 1}, {FileIdx: 0, ArtIdx: 2}, {FileIdx: 1, ArtIdx: 1},
		}
	}
	tests := []struct {
		name      string
		verdict   durability.FileVerdict
		wantRows  [][2]int
		wantDirty []int
	}{
		{"DeleteAll drops the file's rows and dirty entry only",
			durability.FileVerdict{FileIdx: 0, DeleteAll: true}, [][2]int{{1, 1}}, []int{1}},
		{"DeleteArtIdxs drops only the named art in that file and keeps dirty",
			durability.FileVerdict{FileIdx: 0, DeleteArtIdxs: []int32{1}}, [][2]int{{0, 2}, {1, 1}}, []int{0, 1}},
		{"ClearComplete keeps dirty and drops no rows",
			durability.FileVerdict{FileIdx: 0, ClearComplete: true}, [][2]int{{0, 1}, {0, 2}, {1, 1}}, []int{0, 1}},
		{"SetComplete keeps dirty and drops no rows",
			durability.FileVerdict{FileIdx: 0, SetComplete: true}, [][2]int{{0, 1}, {0, 2}, {1, 1}}, []int{0, 1}},
		{"an empty verdict changes nothing",
			durability.FileVerdict{FileIdx: 0}, [][2]int{{0, 1}, {0, 2}, {1, 1}}, []int{0, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := newTestJob(t, "id")
			r := newRecorder(&fakeRecordStore{}, func(string) *job.Job { return j }, nil, slog.Default())
			r.pending[j] = rows()
			r.dirty[j] = map[int]durability.FileState{0: {FileIdx: 0}, 1: {FileIdx: 1}}
			r.purgeLocked(j, tc.verdict)
			got := make([][2]int, 0, len(r.pending[j]))
			for _, row := range r.pending[j] {
				got = append(got, [2]int{row.FileIdx, int(row.ArtIdx)})
			}
			if len(got) != len(tc.wantRows) {
				t.Fatalf("rows = %v, want %v", got, tc.wantRows)
			}
			for i := range got {
				if got[i] != tc.wantRows[i] {
					t.Fatalf("rows = %v, want %v", got, tc.wantRows)
				}
			}
			if len(r.dirty[j]) != len(tc.wantDirty) {
				t.Fatalf("dirty = %v, want files %v", r.dirty[j], tc.wantDirty)
			}
			for _, f := range tc.wantDirty {
				if _, ok := r.dirty[j][f]; !ok {
					t.Errorf("dirty = %v, want file %d kept", r.dirty[j], f)
				}
			}
		})
	}
}

func TestRecorder_PurgeLockedCompleteVerdictKeepsFileStateButOverridesComplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    durability.FileVerdict
		want bool
	}{
		{"ClearComplete", durability.FileVerdict{FileIdx: 0, ClearComplete: true}, false},
		{"SetComplete", durability.FileVerdict{FileIdx: 0, SetComplete: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newTestJob(t, "id")
			r := newRecorder(&fakeRecordStore{}, func(string) *job.Job { return j }, nil, slog.Default())
			r.dirty[j] = map[int]durability.FileState{0: {FileIdx: 0, Complete: !tc.want, Filename: "a.bin", FetchPolicy: 2}}
			r.purgeLocked(j, tc.v)
			got, ok := r.dirty[j][0]
			if !ok {
				t.Fatal("dirty entry dropped, want it kept")
			}
			if got.Filename != "a.bin" || got.FetchPolicy != 2 || got.Complete != tc.want {
				t.Errorf("state = %+v, want filename and policy kept and Complete=%v", got, tc.want)
			}
		})
	}
}

func TestRecorder_PurgeLockedLeavesNoEmptyDirtyEntry(t *testing.T) {
	j := newTestJob(t, "id")
	r := newRecorder(&fakeRecordStore{}, func(string) *job.Job { return j }, nil, slog.Default())
	r.dirty[j] = map[int]durability.FileState{0: {FileIdx: 0}}
	r.purgeLocked(j, durability.FileVerdict{FileIdx: 0, DeleteAll: true})
	if _, ok := r.dirty[j]; ok {
		t.Error("dirty kept an empty map for the job")
	}
}

func TestRecorder_PurgeLockedLeavesNoEmptyPendingEntry(t *testing.T) {
	j := newTestJob(t, "id")
	r := newRecorder(&fakeRecordStore{}, func(string) *job.Job { return j }, nil, slog.Default())
	r.pending[j] = []durability.WrittenRow{{FileIdx: 0, ArtIdx: 1}}
	r.purgeLocked(j, durability.FileVerdict{FileIdx: 0, DeleteAll: true})
	if _, ok := r.pending[j]; ok {
		t.Error("pending kept an empty entry for the job")
	}
}

func TestRecorder_FlushLockedWritesNothingWhenIdleAndRemergesOnError(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, nil, slog.Default())
	if err := r.lockWriter(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.unlockWriter()
	if err := r.flushLocked(context.Background()); err != nil || len(st.snapshot()) != 0 {
		t.Fatalf("idle flushLocked = %v with %d batches, want nil and none", err, len(st.snapshot()))
	}
	st.failNext = true
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	if err := r.flushLocked(context.Background()); err == nil {
		t.Fatal("want the store error")
	}
	if got := len(r.pending[j]); got != 1 {
		t.Errorf("pending rows after a failed flushLocked = %d, want the snapshot re-merged (1)", got)
	}
}

func TestRecorder_RunDoesNotFlushOnCancel(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, nil, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { r.run(ctx, time.Hour); close(finished) }()
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	cancel()
	<-finished
	if n := st.rowCount(); n != 0 {
		t.Errorf("rows = %d, want 0: run must not flush when cancelled", n)
	}
}

func TestRecorder_RunFlushesOnTick(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, nil, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { r.run(ctx, time.Millisecond); close(finished) }()
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	deadline := time.Now().Add(30 * time.Second)
	for st.rowCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-finished
	if n := st.rowCount(); n != 1 {
		t.Errorf("rows = %d, want the tick to have flushed 1", n)
	}
}
