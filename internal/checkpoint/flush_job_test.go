package checkpoint

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// failForIDStore fails every batch that carries failID and records the rest.
type failForIDStore struct {
	failID string
	mu     sync.Mutex
	ok     [][]string
}

func (s *failForIDStore) SaveBatch(_ context.Context, cps []job.Checkpoint) error {
	ids := make([]string, 0, len(cps))
	for _, cp := range cps {
		ids = append(ids, cp.ID)
	}
	if slices.Contains(ids, s.failID) {
		return errSaveBatchFailed
	}
	slices.Sort(ids)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ok = append(s.ok, ids)
	return nil
}

func (s *failForIDStore) written() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ok)
}

// blockOnIDStore holds open every batch that carries blockID until release is
// closed, and lets every other batch through at once.
type blockOnIDStore struct {
	blockID string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	ok      [][]string
}

func newBlockOnIDStore(id string) *blockOnIDStore {
	return &blockOnIDStore{blockID: id, entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockOnIDStore) SaveBatch(_ context.Context, cps []job.Checkpoint) error {
	ids := make([]string, 0, len(cps))
	for _, cp := range cps {
		ids = append(ids, cp.ID)
	}
	if slices.Contains(ids, s.blockID) {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ok = append(s.ok, ids)
	return nil
}

func (s *blockOnIDStore) written() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ok)
}

// TestWrite_EmptyTakeDoesNotWrite: a take that yields nothing reaches no store
// and publishes no batch for Prune to wait on.
func TestWrite_EmptyTakeDoesNotWrite(t *testing.T) {
	t.Parallel()
	st := &recordingStore{}
	c := New(st, time.Hour, nil)

	if err := c.write(context.Background(), func() map[string]*job.Job { return nil }); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(st.batches) != 0 {
		t.Fatalf("batches = %v, want none", st.batches)
	}
	if c.flushDone != nil || c.flushing != nil {
		t.Fatal("an empty write left a batch published for Prune to wait on")
	}
}

// TestWrite_PublishesTheBatchWhileWriting: the taken batch is in inFlight and
// flushing for exactly the duration of SaveBatch.
func TestWrite_PublishesTheBatchWhileWriting(t *testing.T) {
	t.Parallel()
	st := newBlockOnIDStore("a")
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(st.release) }) })
	c := New(st, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))

	wrote := make(chan error, 1)
	go func() {
		wrote <- c.write(context.Background(), func() map[string]*job.Job {
			return map[string]*job.Job{"a": a}
		})
	}()
	<-st.entered

	c.mu.Lock()
	inFlight, flushing, done := c.inFlight["a"], c.flushing["a"], c.flushDone
	c.mu.Unlock()
	if inFlight != a || flushing != a || done == nil {
		t.Fatalf("during the write: inFlight=%v flushing=%v flushDone set=%v; want the batch published",
			inFlight, flushing, done != nil)
	}

	release.Do(func() { close(st.release) })
	if err := <-wrote; err != nil {
		t.Fatalf("write: %v", err)
	}
	<-done
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.inFlight) != 0 || c.flushing != nil || c.flushDone != nil {
		t.Fatalf("after the write: inFlight=%v flushing=%v; want both cleared", c.inFlight, c.flushing)
	}
}

// TestFlushJob_WritesOnlyThatJob: the other job's mark is neither written nor
// dropped, so the next Flush still writes it.
func TestFlushJob_WritesOnlyThatJob(t *testing.T) {
	t.Parallel()
	st := &recordingStore{}
	c := New(st, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))
	b := job.New("b", "B", job.PolicyFromPP(3))
	c.Mark(a)
	c.Mark(b)

	if err := c.FlushJob(context.Background(), a); err != nil {
		t.Fatalf("FlushJob: %v", err)
	}
	if len(st.batches) != 1 || len(st.batches[0]) != 1 || st.batches[0][0].ID != "a" {
		t.Fatalf("batches = %v, want one batch holding only a", st.batches)
	}
	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1: FlushJob should take a and leave b", got)
	}

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(st.batches) != 2 || len(st.batches[1]) != 1 || st.batches[1][0].ID != "b" {
		t.Fatalf("batches = %v, want the second to hold only b", st.batches)
	}
}

// TestFlushJob_AnotherJobsFailureDoesNotFailIt: a store that cannot write some
// other job must not fail the flush of this one.
func TestFlushJob_AnotherJobsFailureDoesNotFailIt(t *testing.T) {
	t.Parallel()
	st := &failForIDStore{failID: "other"}
	c := New(st, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(job.New("other", "Other", job.PolicyFromPP(3)))
	c.Mark(a)

	if err := c.FlushJob(context.Background(), a); err != nil {
		t.Fatalf("FlushJob(a) = %v, want nil: another job's write failure failed this job's flush", err)
	}
	if got := st.written(); len(got) != 1 || !slices.Equal(got[0], []string{"a"}) {
		t.Fatalf("written = %v, want exactly [[a]]", got)
	}
	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1: the other job's mark must wait for the next Flush", got)
	}
}

// TestFlushJob_FailedWriteKeepsTheMark: a failed write hands j back to the
// dirty set, so the next flush writes it.
func TestFlushJob_FailedWriteKeepsTheMark(t *testing.T) {
	t.Parallel()
	st := &failingStore{failsLeft: 1}
	c := New(st, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(a)

	if err := c.FlushJob(context.Background(), a); !errors.Is(err, errSaveBatchFailed) {
		t.Fatalf("FlushJob: got %v, want errSaveBatchFailed", err)
	}
	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1: a failed FlushJob lost the job's mark", got)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(st.batches) != 1 || len(st.batches[0]) != 1 || st.batches[0][0].ID != "a" {
		t.Fatalf("batches = %v, want one batch holding a", st.batches)
	}
}

// TestFlushJob_LeavesALaterInstanceUnderTheSameID: flushing a departed
// instance must neither write nor take a later instance's pending mark.
func TestFlushJob_LeavesALaterInstanceUnderTheSameID(t *testing.T) {
	t.Parallel()
	st := &recordingStore{}
	c := New(st, time.Hour, nil)
	old := job.New("a", "Old", job.PolicyFromPP(3))
	retry := job.New("a", "Retry", job.PolicyFromPP(3))
	c.Mark(retry)

	if err := c.FlushJob(context.Background(), old); err != nil {
		t.Fatalf("FlushJob: %v", err)
	}
	if len(st.batches) != 0 {
		t.Fatalf("FlushJob of the old instance wrote %v, want nothing", st.batches)
	}
	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1: FlushJob of the old instance took the retry's mark", got)
	}
}

// TestFlushJob_DoesNotWriteAPrunedInstance: a pruned instance has nothing
// FlushJob may write, however it was marked.
func TestFlushJob_DoesNotWriteAPrunedInstance(t *testing.T) {
	t.Parallel()
	st := &recordingStore{}
	c := New(st, time.Hour, nil)
	old := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(old)
	c.Prune(old)
	c.Mark(old) // late, refused

	if err := c.FlushJob(context.Background(), old); err != nil {
		t.Fatalf("FlushJob: %v", err)
	}
	if len(st.batches) != 0 {
		t.Fatalf("FlushJob wrote %v for a pruned instance, want nothing", st.batches)
	}
}

// TestFlushJob_KeepsAFailedFlushsMarks: a FlushJob that runs while a Flush is
// failing must leave the jobs that Flush re-merges in the dirty set.
func TestFlushJob_KeepsAFailedFlushsMarks(t *testing.T) {
	t.Parallel()
	st := &failingStore{failsLeft: 1}
	c := New(st, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))
	b := job.New("b", "B", job.PolicyFromPP(3))
	c.Mark(b)

	jobDone := make(chan error, 1)
	st.beforeFail = func() {
		c.Mark(a)
		go func() { jobDone <- c.FlushJob(context.Background(), a) }()
	}
	if err := c.Flush(context.Background()); !errors.Is(err, errSaveBatchFailed) {
		t.Fatalf("Flush: got %v, want errSaveBatchFailed", err)
	}
	if err := <-jobDone; err != nil {
		t.Fatalf("FlushJob: %v", err)
	}
	if len(st.batches) != 1 || len(st.batches[0]) != 1 || st.batches[0][0].ID != "a" {
		t.Fatalf("batches = %v, want one batch holding only a", st.batches)
	}
	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1: the failed Flush's b was dropped by FlushJob", got)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(st.batches) != 2 || st.batches[1][0].ID != "b" {
		t.Fatalf("batches = %v, want the second to hold b", st.batches)
	}
}

// TestFlushJob_WaitsForAFlushAlreadyWritingTheJob: FlushJob returning nil means
// j is on disk, even when a Flush took j out of the dirty set first.
func TestFlushJob_WaitsForAFlushAlreadyWritingTheJob(t *testing.T) {
	t.Parallel()
	st := newBlockOnIDStore("a")
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(st.release) }) })
	c := New(st, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(a)

	flushed := make(chan error, 1)
	go func() { flushed <- c.Flush(context.Background()) }()
	<-st.entered

	jobDone := make(chan error, 1)
	go func() { jobDone <- c.FlushJob(context.Background(), a) }()
	select {
	case <-jobDone:
		t.Fatal("FlushJob returned while the Flush carrying the job was still inside " +
			"SaveBatch, so its caller proceeds with the job's rows not yet on disk")
	case <-time.After(200 * time.Millisecond):
	}

	release.Do(func() { close(st.release) })
	if err := <-jobDone; err != nil {
		t.Fatalf("FlushJob: %v", err)
	}
	if err := <-flushed; err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// TestFlushJob_WritesAJobAFailedFlushReMerged: when the Flush that took j
// fails, the FlushJob waiting behind it writes j itself.
func TestFlushJob_WritesAJobAFailedFlushReMerged(t *testing.T) {
	t.Parallel()
	st := &failingStore{failsLeft: 1}
	c := New(st, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(a)

	jobDone := make(chan error, 1)
	st.beforeFail = func() {
		go func() { jobDone <- c.FlushJob(context.Background(), a) }()
		// Give a FlushJob that does not wait for this Flush time to return, so
		// that version fails below rather than passing by luck.
		select {
		case err := <-jobDone:
			jobDone <- err
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := c.Flush(context.Background()); !errors.Is(err, errSaveBatchFailed) {
		t.Fatalf("Flush: got %v, want errSaveBatchFailed", err)
	}
	if err := <-jobDone; err != nil {
		t.Fatalf("FlushJob: %v", err)
	}
	if len(st.batches) != 1 || len(st.batches[0]) != 1 || st.batches[0][0].ID != "a" {
		t.Fatalf("batches = %v, want FlushJob to have written a after the failed Flush", st.batches)
	}
	if got := c.DirtyCount(); got != 0 {
		t.Fatalf("DirtyCount = %d, want 0", got)
	}
}

// TestPrune_WaitsForAFlushAcrossAConcurrentFlushJob: a FlushJob that arrives
// while a Flush is writing must not end Prune's wait for that Flush.
func TestPrune_WaitsForAFlushAcrossAConcurrentFlushJob(t *testing.T) {
	t.Parallel()
	st := newBlockOnIDStore("b")
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(st.release) }) })
	c := New(st, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))
	b := job.New("b", "B", job.PolicyFromPP(3))
	c.Mark(b)

	flushed := make(chan error, 1)
	go func() { flushed <- c.Flush(context.Background()) }()
	<-st.entered

	c.Mark(a)
	jobDone := make(chan error, 1)
	go func() { jobDone <- c.FlushJob(context.Background(), a) }()
	// Give a FlushJob that does not wait for the Flush time to finish, so that
	// version fails below rather than passing by luck.
	select {
	case err := <-jobDone:
		jobDone <- err
	case <-time.After(200 * time.Millisecond):
	}

	pruned := make(chan struct{})
	go func() {
		c.Prune(b)
		close(pruned)
	}()
	select {
	case <-pruned:
		t.Fatal("Prune(b) returned while the Flush carrying b was still inside SaveBatch; " +
			"a FlushJob in between ended the wait, and the departure's reclaim now races " +
			"the batch")
	case <-time.After(100 * time.Millisecond):
	}

	release.Do(func() { close(st.release) })
	<-pruned
	if err := <-flushed; err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := <-jobDone; err != nil {
		t.Fatalf("FlushJob: %v", err)
	}
	if got := st.written(); len(got) != 2 {
		t.Fatalf("written = %v, want b's batch and a's", got)
	}
}

// TestPrune_WaitsForAFlushJobCarryingTheJob: FlushJob's batch is one Prune
// waits for, like Flush's.
func TestPrune_WaitsForAFlushJobCarryingTheJob(t *testing.T) {
	t.Parallel()
	st := newBlockOnIDStore("a")
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(st.release) }) })
	c := New(st, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(a)

	jobDone := make(chan error, 1)
	go func() { jobDone <- c.FlushJob(context.Background(), a) }()
	<-st.entered

	pruned := make(chan struct{})
	go func() {
		c.Prune(a)
		close(pruned)
	}()
	waitUntilNotInFlight(t, c, "a")
	select {
	case <-pruned:
		t.Fatal("Prune returned while the FlushJob carrying the job was still inside SaveBatch")
	case <-time.After(100 * time.Millisecond):
	}

	release.Do(func() { close(st.release) })
	<-pruned
	if err := <-jobDone; err != nil {
		t.Fatalf("FlushJob: %v", err)
	}
}
