package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
			// first flush has no instance answer for it.
			r.noteWritten(j2, durability.WrittenRow{FileIdx: 0, ArtIdx: 3, Length: 10})
			r.markDirty(j2, 0, durability.FileState{Complete: true})
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
	var rows, files int
	for _, b := range st.snapshot() {
		if b.JobID == "two" {
			rows += len(b.Rows)
			files += len(b.Files)
		}
	}
	if rows != 1 || files != 1 {
		t.Errorf("job two after the second flush: %d rows, %d files, want 1 and 1 — state buffered during a listing was dropped", rows, files)
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
