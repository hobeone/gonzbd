package app

import (
	"context"
	"errors"
	"log/slog"
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
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10}, 10, "srv")
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Length: 10}, 10, "srv")
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

// TestRecorder_CompleteNeverPrecedesItsRowUnderConcurrency has a writer note
// file i's row and then mark it complete, without pause, across a fixed number
// of flushes, and requires every flushed complete flag to be accompanied, in
// that flush or an earlier one, by its row.
func TestRecorder_CompleteNeverPrecedesItsRowUnderConcurrency(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())

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
			r.noteWritten(j, durability.WrittenRow{FileIdx: i, ArtIdx: 0, Length: 1}, 1, "srv")
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
			r := newRecorder(&fakeRecordStore{}, func(string) *job.Job { return j }, slog.Default())
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
			r := newRecorder(&fakeRecordStore{}, func(string) *job.Job { return j }, slog.Default())
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
	r := newRecorder(&fakeRecordStore{}, func(string) *job.Job { return j }, slog.Default())
	r.dirty[j] = map[int]durability.FileState{0: {FileIdx: 0}}
	r.purgeLocked(j, durability.FileVerdict{FileIdx: 0, DeleteAll: true})
	if _, ok := r.dirty[j]; ok {
		t.Error("dirty kept an empty map for the job")
	}
}

func TestRecorder_PurgeLockedLeavesNoEmptyPendingEntry(t *testing.T) {
	j := newTestJob(t, "id")
	r := newRecorder(&fakeRecordStore{}, func(string) *job.Job { return j }, slog.Default())
	r.pending[j] = []durability.WrittenRow{{FileIdx: 0, ArtIdx: 1}}
	r.purgeLocked(j, durability.FileVerdict{FileIdx: 0, DeleteAll: true})
	if _, ok := r.pending[j]; ok {
		t.Error("pending kept an empty entry for the job")
	}
}

func TestRecorder_FlushLockedWritesNothingWhenIdleAndRemergesOnError(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if err := r.flushLocked(context.Background()); err != nil || len(st.snapshot()) != 0 {
		t.Fatalf("idle flushLocked = %v with %d batches, want nil and none", err, len(st.snapshot()))
	}
	st.failNext = true
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10}, 10, "srv")
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
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { r.run(ctx, time.Hour); close(finished) }()
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10}, 10, "srv")
	cancel()
	<-finished
	if n := st.rowCount(); n != 0 {
		t.Errorf("rows = %d, want 0: run must not flush when cancelled", n)
	}
}

func TestRecorder_RunFlushesOnTick(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { r.run(ctx, time.Millisecond); close(finished) }()
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10}, 10, "srv")
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
