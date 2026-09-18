package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// gatedSaveStore holds the Save of one job until release is closed, then
// returns err, or writes the row when err is nil. Every other job's Save goes
// straight through to the embedded fakeStore.
type gatedSaveStore struct {
	*fakeStore
	id      string
	err     error
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedSaveStore(id string, err error) *gatedSaveStore {
	return &gatedSaveStore{
		fakeStore: &fakeStore{},
		id:        id,
		err:       err,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
}

// waitEntered blocks until the gated Save is reached, failing the test rather
// than hanging if Add never calls it.
func (g *gatedSaveStore) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("Add never reached the store's Save for %s", g.id)
	}
}

func (g *gatedSaveStore) Save(ctx context.Context, p Persisted) error {
	if p.ID == g.id {
		g.once.Do(func() { close(g.entered) })
		<-g.release
		if g.err != nil {
			return g.err
		}
	}
	return g.fakeStore.Save(ctx, p)
}

// TestAdd_WritesTheQueueRowBeforeReturning pins P1's guarantee: a caller that
// acknowledges a job once Add returns has a row behind the acknowledgement,
// with no tick having run.
func TestAdd_WritesTheQueueRowBeforeReturning(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})

	if err := d.Add(context.Background(), j, Header{Name: "n"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	p, ok := st.row("j1")
	if !ok {
		t.Fatal("Add returned with no queue row written; a crash now loses a job the caller was told was added")
	}
	if p.Header.Name != "n" || p.State.State != job.StateUnset {
		t.Errorf("row = %+v, want the never-run row for j1", p)
	}

	// Once Add has returned, the job is the tick's.
	d.tick(context.Background())
	if s := j.Snapshot().State.State; s == job.StateUnset {
		t.Error("the tick skipped j1 after its Add returned; it would never run")
	}
}

// TestAdd_WritesUnderTheCallersContext pins that the write is bounded by the
// context Add is given, which is what lets a caller bound it.
func TestAdd_WritesUnderTheCallersContext(t *testing.T) {
	st := &contextAwareStore{}
	d := newTestDispatcher(t, withStore(st))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := d.Add(ctx, job.New("j1", "n", job.Policy{}), Header{Name: "n"})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Add under a cancelled context = %v, want context.Canceled", err)
	}
	if n := d.Len(); n != 0 {
		t.Errorf("Len = %d after a failed Add, want 0", n)
	}
}

// TestAdd_TickNeverSeesAJobWhoseRowIsUnwritten is design §8.1's unwritten-job
// test. Add's Save blocks across three ticks and then fails. The tick must not
// have touched the job: an attempt, a lease or a hydration would be resources
// the failed Add does not unwind, and a Save by the tick would leave a row for
// a job the caller was told was not added.
//
// The queue holds no other job. Add holds storeMu across its Save, so any
// other job the ticks advanced would block them on its own write and hide
// whether they reached this one.
func TestAdd_TickNeverSeesAJobWhoseRowIsUnwritten(t *testing.T) {
	saveErr := errors.New("disk full")
	st := newGatedSaveStore("j1", saveErr)
	var hydratedMu sync.Mutex
	hydrated := map[string]bool{}
	res := &fakeResidency{onHydrate: func(id string) {
		hydratedMu.Lock()
		defer hydratedMu.Unlock()
		hydrated[id] = true
	}}
	run := &fakeRunner{}
	d := newTestDispatcher(t, withStore(st), withResidency(res), withRunner(run))

	j := job.New("j1", "n", job.Policy{})
	addErr := make(chan error, 1)
	go func() { addErr <- d.Add(context.Background(), j, Header{Name: "n"}) }()
	st.waitEntered(t)

	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		for range 3 {
			d.tick(context.Background())
		}
	}()
	select {
	case <-ticked:
	case <-time.After(2 * time.Second):
		t.Error("a tick blocked behind Add's write: it reached persistIfChanged for a job whose row was not yet written")
	}
	close(st.release)
	<-ticked

	if err := <-addErr; !errors.Is(err, saveErr) {
		t.Fatalf("Add = %v, want the store's error", err)
	}
	if s := j.Snapshot().State.State; s != job.StateUnset {
		t.Errorf("j1 is at %v, want StateUnset: a tick began an attempt on a job whose row was unwritten", s)
	}
	if j.HoldsLease() {
		t.Error("j1 holds a lease that no one will return")
	}
	hydratedMu.Lock()
	if hydrated["j1"] {
		t.Error("j1 was hydrated while its row was unwritten")
	}
	hydratedMu.Unlock()
	if run.started("j1") {
		t.Error("a worker launched for j1 while its row was unwritten")
	}
	if _, ok := st.row("j1"); ok {
		t.Error("the store holds a row for j1, whose Add failed")
	}
	if rows := d.List(); len(rows) != 0 {
		t.Errorf("List = %v, want empty: a failed Add must leave nothing registered", rows)
	}
}

// TestAdd_AbortedRemovalDuringTheWriteLeavesTheJobVisible covers a removal that
// begins while Add's write is in flight and then aborts, as Remove does when
// its store delete fails. markWritten refuses to record a job under a removal
// marker, so the job ends Add registered but with no written entry; the tick
// must still reach it, or it is never evicted or persisted again.
func TestAdd_AbortedRemovalDuringTheWriteLeavesTheJobVisible(t *testing.T) {
	st := newGatedSaveStore("j1", nil)
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})

	addErr := make(chan error, 1)
	go func() { addErr <- d.Add(context.Background(), j, Header{Name: "n"}) }()
	st.waitEntered(t)
	rm, ok := d.beginRemoval("j1")
	if !ok {
		t.Fatal("setup: j1 is not registered")
	}
	close(st.release)
	if err := <-addErr; err != nil {
		t.Fatalf("Add: %v", err)
	}
	rm.abort()

	d.tick(context.Background())

	if s := j.Snapshot().State.State; s == job.StateUnset {
		t.Error("the tick never reached j1 after the removal aborted; a registered job " +
			"is stranded, never advanced, evicted or persisted again")
	}
}

// TestAdd_KicksTheTickAfterThePersist pins Add's second kick. register kicks
// before the write, and a tick that takes that wake while the write is in
// flight skips the job as unwritten; without a kick after the write, the job
// waits for the ticker.
func TestAdd_KicksTheTickAfterThePersist(t *testing.T) {
	st := newGatedSaveStore("j1", nil)
	d := newTestDispatcher(t, withStore(st))

	addErr := make(chan error, 1)
	go func() { addErr <- d.Add(context.Background(), job.New("j1", "n", job.Policy{}), Header{}) }()
	st.waitEntered(t)
	// Stand in for a tick that consumed register's wake mid-write.
	select {
	case <-d.wake:
	default:
		t.Fatal("setup: register did not kick")
	}
	close(st.release)
	if err := <-addErr; err != nil {
		t.Fatalf("Add: %v", err)
	}

	select {
	case <-d.wake:
	default:
		t.Error("no wake is pending after Add returned; the job waits for the next ticker tick")
	}
}

// TestStart_AfterAddKeepsTheJobRegisteredOnce pins restore's handling of a job
// an Add before Start already wrote: the row is the one this process wrote, so
// restore leaves the live registration alone instead of refusing a duplicate.
func TestStart_AfterAddKeepsTheJobRegisteredOnce(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{Name: "n"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start after an Add: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	if n := d.Len(); n != 1 {
		t.Errorf("Len = %d, want 1", n)
	}
	if got, _ := d.Job("j1"); got != j {
		t.Error("the registry holds a rebuilt job rather than the one Add registered")
	}
}

// TestStart_RefusesARowThatDiffersFromTheOneAddWrote pins that restore's skip
// is narrow: a stored row for a registered job that is not the row this
// process wrote is still refused.
func TestStart_RefusesARowThatDiffersFromTheOneAddWrote(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	if err := d.Add(context.Background(), job.New("j1", "n", job.Policy{}), Header{Name: "n"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p, _ := st.row("j1")
	p.Header.Name = "someone else's"
	st.seed([]Persisted{p})

	if err := d.Start(context.Background()); err == nil {
		t.Fatal("Start registered a stored row that differs from the one Add wrote for the same job")
	}
}
