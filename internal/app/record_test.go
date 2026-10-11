package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// newTestJob builds a one-file, six-article job under id. Two calls with the
// same id return distinct instances.
func newTestJob(t *testing.T, id string) *job.Job {
	t.Helper()
	j := job.New(id, id, job.PolicyFromPP(3))
	arts := make([]job.JobArticle, 6)
	for i := range arts {
		arts[i] = job.JobArticle{ID: fmt.Sprintf("a%d@example.com", i), Bytes: 100, Number: i + 1}
	}
	if err := j.AttachContent(job.NewManifest([]job.JobFile{{Subject: "f.bin", Bytes: 600, Articles: arts}})); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j
}

// fakeRecordStore keeps the batches it was handed.
type fakeRecordStore struct {
	mu       sync.Mutex
	batches  []durability.RecordBatch
	failNext bool
}

func (s *fakeRecordStore) ApplyRecord(_ context.Context, b []durability.RecordBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext {
		s.failNext = false
		return errors.New("boom")
	}
	s.batches = append(s.batches, b...)
	return nil
}

func (s *fakeRecordStore) rowCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.batches {
		n += len(b.Rows)
	}
	return n
}

func (s *fakeRecordStore) snapshot() []durability.RecordBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]durability.RecordBatch(nil), s.batches...)
}

func TestRecorder_DropsRowsOfAReplacedInstance(t *testing.T) {
	st := &fakeRecordStore{}
	old, cur := newTestJob(t, "id"), newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return cur }, slog.Default())
	r.noteWritten(old, durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Length: 10})
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := st.rowCount(); n != 0 {
		t.Errorf("flushed %d rows from a job instance a retry replaced, want 0", n)
	}
}

func TestRecorder_ApplyIgnoresTheInstanceCheck(t *testing.T) {
	st := &fakeRecordStore{}
	rebuilt := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return nil }, slog.Default()) // not yet Added
	if err := r.apply(context.Background(), rebuilt, []durability.FileVerdict{{FileIdx: 0, ClearComplete: true}}); err != nil {
		t.Fatal(err)
	}
	if len(st.batches) != 1 || len(st.batches[0].Verdicts) != 1 {
		t.Errorf("batches = %+v, want the retry's verdict committed before Add", st.batches)
	}
}

func TestRecorder_CompleteNeverLandsWithoutItsLastRow(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 4, Length: 10})
	r.markDirty(j, 0, durability.FileState{FileIdx: 0, Complete: true})
	_ = r.flush(context.Background())
	b := st.batches[0]
	if len(b.Rows) != 1 || len(b.Files) != 1 {
		t.Errorf("batch = %+v, want the row and the complete flag in one transaction", b)
	}
}

func TestRecorder_FlushesADirtyOnlyJob(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.markDirty(j, 0, durability.FileState{Complete: true, Filename: "f.bin"})
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := st.snapshot()
	if len(b) != 1 || len(b[0].Files) != 1 || !b[0].Files[0].Complete || len(b[0].Rows) != 0 {
		t.Errorf("batches = %+v, want one batch carrying file 0's state and no rows", b)
	}
}

func TestRecorder_LeavesAJobFirstBufferedAfterTheListingForTheNextFlush(t *testing.T) {
	st := &fakeRecordStore{}
	j1, j2 := newTestJob(t, "one"), newTestJob(t, "two")
	var r *recorder
	calls := 0
	r = newRecorder(st, func(id string) *job.Job {
		calls++
		if calls == 1 {
			// j2 is buffered after liveInstances listed the jobs, so the
			// first flush has no instance answer for it. Its untrust of file
			// 1 cannot take wmu (this flush holds it) and leaves the delete
			// owed.
			r.noteWritten(j2, durability.WrittenRow{FileIdx: 0, ArtIdx: 3, Length: 10})
			r.markDirty(j2, 0, durability.FileState{Complete: true})
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := r.untrust(cancelled, j2, 1); !errors.Is(err, context.Canceled) {
				t.Errorf("untrust during the flush = %v, want Canceled", err)
			}
		}
		if id == "one" {
			return j1
		}
		return j2
	}, slog.Default())
	r.noteWritten(j1, durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Length: 10})

	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, b := range st.snapshot() {
		if b.JobID == "two" {
			t.Fatalf("first flush wrote %+v for a job buffered after its listing", b)
		}
	}
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rows, files, owed int
	for _, b := range st.snapshot() {
		if b.JobID == "two" {
			rows += len(b.Rows)
			files += len(b.Files)
			for _, v := range b.Verdicts {
				if v.FileIdx == 1 && v.DeleteAll {
					owed++
				}
			}
		}
	}
	if rows != 1 || files != 1 || owed != 1 {
		t.Errorf("job two after the second flush: %d rows, %d files, %d owed deletes, want 1 of each — state buffered during a listing was dropped", rows, files, owed)
	}
}

func TestRecorder_RemergesOnError(t *testing.T) {
	st := &fakeRecordStore{failNext: true}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	if err := r.flush(context.Background()); err == nil {
		t.Fatal("want the store error")
	}
	_ = r.flush(context.Background())
	if n := st.rowCount(); n != 1 {
		t.Errorf("rows after a retried flush = %d, want 1", n)
	}
}

// failingOnce marks a newer state for file 0 while the first ApplyRecord is in
// flight, then fails it.
type failingOnce struct {
	fakeRecordStore
	during func()
}

func (s *failingOnce) ApplyRecord(ctx context.Context, b []durability.RecordBatch) error {
	if s.during != nil {
		d := s.during
		s.during = nil
		d()
		return errors.New("boom")
	}
	return s.fakeRecordStore.ApplyRecord(ctx, b)
}

func TestRecorder_RemergeKeepsNewerFileStateAndRowOrder(t *testing.T) {
	st := &failingOnce{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	r.markDirty(j, 0, durability.FileState{Complete: false, Filename: "old"})
	st.during = func() {
		r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 20})
		r.markDirty(j, 0, durability.FileState{Complete: true, Filename: "new"})
	}
	if err := r.flush(context.Background()); err == nil {
		t.Fatal("want the store error")
	}
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := st.batches[0]
	if len(b.Files) != 1 || b.Files[0].Filename != "new" || !b.Files[0].Complete {
		t.Errorf("files = %+v, want the newer state for file 0", b.Files)
	}
	if len(b.Rows) != 2 || b.Rows[0].Length != 10 || b.Rows[1].Length != 20 {
		t.Errorf("rows = %+v, want the older row first so the newer one wins on replace", b.Rows)
	}
}

func TestRecorder_UntrustPurgesPendingRows(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Length: 10})
	r.noteWritten(j, durability.WrittenRow{FileIdx: 1, ArtIdx: 0, Length: 10})
	r.markDirty(j, 0, durability.FileState{Complete: true})
	if err := r.apply(context.Background(), j, []durability.FileVerdict{{FileIdx: 0, DeleteAll: true, ClearComplete: true}}); err != nil {
		t.Fatal(err)
	}
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, b := range st.snapshot() {
		for _, row := range b.Rows {
			if row.FileIdx == 0 {
				t.Errorf("batch carries a row for the untrusted file 0: %+v", row)
			}
		}
		for _, f := range b.Files {
			if f.FileIdx == 0 {
				t.Errorf("batch carries dirty state for the untrusted file 0: %+v", f)
			}
		}
	}
	if n := st.rowCount(); n != 1 {
		t.Errorf("rows = %d, want only file 1's row", n)
	}
}

// owedOf is the owed deletes the recorder holds for j, by file.
func owedOf(r *recorder, j *job.Job) map[int]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.owed[j])
}

// TestRecorder_UntrustWhoseWriteFailsOwesTheDeleteToTheNextFlush pins the
// write-failure half of #827: the untrust's own ApplyRecord fails, so the
// next flush carries the DeleteAll ahead of the rows noted since, leaves
// complete to the file's newer state in the same batch, and settles the debt.
func TestRecorder_UntrustWhoseWriteFailsOwesTheDeleteToTheNextFlush(t *testing.T) {
	st := &fakeRecordStore{failNext: true}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	r.noteWritten(j, durability.WrittenRow{FileIdx: 1, ArtIdx: 0, Length: 10})
	if err := r.untrust(context.Background(), j, 0); err == nil {
		t.Fatal("want the injected store error")
	}
	if got := owedOf(r, j); len(got) != 1 || got[0] == 0 {
		t.Fatalf("owed after the failed untrust = %v, want file 0", got)
	}

	// The refetch of article 1 and the file's completion.
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 20})
	r.markDirty(j, 0, durability.FileState{FileIdx: 0, Complete: true})
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := st.snapshot()
	if len(b) != 1 {
		t.Fatalf("batches = %+v, want one", b)
	}
	if v := b[0].Verdicts; len(v) != 1 || v[0].FileIdx != 0 || !v[0].DeleteAll || v[0].ClearComplete {
		t.Errorf("verdicts = %+v, want file 0's DeleteAll alone, with complete left to the batch's file state", v)
	}
	for _, row := range b[0].Rows {
		if row.FileIdx == 0 && row.Length != 20 {
			t.Errorf("the batch carries a row noted before the untrust: %+v", row)
		}
	}
	if len(b[0].Rows) != 2 || len(b[0].Files) != 1 || !b[0].Files[0].Complete {
		t.Errorf("batch = %+v, want file 1's row, the refetched row and file 0 complete", b[0])
	}
	if got := owedOf(r, j); len(got) != 0 {
		t.Errorf("owed after the flush that carried it = %v, want none", got)
	}
}

// TestRecorder_OwedDeleteIsWrittenOnItsOwn pins that a debt with nothing else
// buffered still gets a batch, with ClearComplete since no file state is in
// it, and is settled by it.
func TestRecorder_OwedDeleteIsWrittenOnItsOwn(t *testing.T) {
	st := &fakeRecordStore{failNext: true}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	if err := r.untrust(context.Background(), j, 0); err == nil {
		t.Fatal("want the injected store error")
	}
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := st.snapshot()
	if len(b) != 1 || len(b[0].Rows) != 0 || len(b[0].Files) != 0 {
		t.Fatalf("batches = %+v, want one carrying the verdict alone", b)
	}
	if v := b[0].Verdicts; len(v) != 1 || v[0].FileIdx != 0 || !v[0].DeleteAll || !v[0].ClearComplete {
		t.Errorf("verdicts = %+v, want file 0's DeleteAll + ClearComplete", v)
	}
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.snapshot(); len(got) != 1 {
		t.Errorf("a settled debt was written again: %+v", got[1:])
	}
}

// TestRecorder_DropsTheDebtOfAReplacedInstance pins the instance check on the
// owed deletes: a retry under the same ID verifies every row itself, so the
// old instance's debt is dropped rather than carried or kept.
func TestRecorder_DropsTheDebtOfAReplacedInstance(t *testing.T) {
	st := &fakeRecordStore{failNext: true}
	old, cur := newTestJob(t, "id"), newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return cur }, slog.Default())
	if err := r.untrust(context.Background(), old, 0); err == nil {
		t.Fatal("want the injected store error")
	}
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.snapshot(); len(got) != 0 {
		t.Errorf("a replaced instance's debt was written: %+v", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.owed) != 0 {
		t.Errorf("owed = %v after the flush, want the replaced instance's entry dropped", r.owed)
	}
}

func TestRecorder_UntrustArtIdxsPurgesOnlyNamedRows(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Length: 10})
	if err := r.apply(context.Background(), j, []durability.FileVerdict{{FileIdx: 0, DeleteArtIdxs: []int32{1}}}); err != nil {
		t.Fatal(err)
	}
	_ = r.flush(context.Background())
	var got []int32
	for _, b := range st.snapshot() {
		for _, row := range b.Rows {
			got = append(got, row.ArtIdx)
		}
	}
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("flushed art idxs = %v, want [2]", got)
	}
}

func TestRecorder_NoteWrittenKeepsRowForNonResidentJob(t *testing.T) {
	st := &fakeRecordStore{}
	bare := job.New("bare", "bare", job.PolicyFromPP(3)) // no content attached
	r2 := newRecorder(st, func(string) *job.Job { return bare }, slog.Default())
	r2.noteWritten(bare, durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Length: 1})
	if err := r2.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := st.rowCount(); n != 1 {
		t.Errorf("rows = %d, want the non-resident job's row kept", n)
	}
}

// dirtyJobs is how many job instances have a file state buffered in r.
func dirtyJobs(r *recorder) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dirty)
}

// TestUntrustTimeout_LeavesTheCloseItsBudget pins the order of the two bounds:
// an untrust runs inside CloseJobHandles, so its own bound must end before the
// close's does.
func TestUntrustTimeout_LeavesTheCloseItsBudget(t *testing.T) {
	t.Parallel()
	if untrustTimeout >= closeHandlesTimeout {
		t.Errorf("untrustTimeout = %v, want less than closeHandlesTimeout (%v): one untrust in a close would use the whole close budget", untrustTimeout, closeHandlesTimeout)
	}
}
