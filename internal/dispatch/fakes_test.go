package dispatch

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/sched"
)

type stubWorkers struct {
	aborted []string
	onAbort func(string)
}

func (s *stubWorkers) Abort(jobID string) {
	s.aborted = append(s.aborted, jobID)
	if s.onAbort != nil {
		s.onAbort(jobID)
	}
}

func testClock() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// fakeResidency is the Residency test double. failOn lets a test force
// Hydrate to fail for one job ID, for Task 4's hydration-failure test.
// onHydrate lets Task 5's tests interleave a Cancel with the unlocked
// residency read.
type fakeResidency struct {
	mu        sync.Mutex
	live      map[string]bool
	failOn    map[string]error
	onHydrate func(string)
}

func (f *fakeResidency) Hydrate(_ context.Context, id string) error {
	if f.onHydrate != nil {
		// Called WITHOUT f.mu: the hook re-enters Dispatcher.Cancel, and
		// holding a fake's lock across that re-entry deadlocks the test
		// against itself.
		f.onHydrate(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[id]; err != nil {
		return err
	}
	if f.live == nil {
		f.live = map[string]bool{}
	}
	f.live[id] = true
	return nil
}

func (f *fakeResidency) Evict(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, id)
}

func (f *fakeResidency) resident(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id]
}

// fakeStore is the Store test double. Task 7 adds seed/row/loadErr; Task 2
// needs only enough to satisfy the interface and let a test observe what was
// saved or deleted.
type fakeStore struct {
	mu      sync.Mutex
	rows    map[string]Persisted
	gone    map[string]bool
	order   []string
	loadErr error
	saveErr error
	delErr  error
	saves   int
	// loadHook runs inside Load, which is how a test reaches the middle of a
	// restore: the whole of restore is the window in which the registry is
	// half-populated, and Load is the one point inside it a fake controls.
	loadHook func()
}

// rows and order are two views of one set and are always written together:
// order fixes the sequence Load returns (rows, a map, has none), and every
// ID in order has an entry in rows. Letting them drift is not a harmless
// test-double shortcut — Load indexes rows by each ID in order, so an ID
// left in order after its row is gone yields a zero-valued Persisted rather
// than no row at all, which is a plausible-looking restore input that no
// production path can produce.

// seed replaces the store's contents with ps, in the given order. It exists
// for Task 7's restore tests, which need Load to return rows in a specific
// sequence.
func (f *fakeStore) seed(ps []Persisted) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = nil
	f.rows = map[string]Persisted{}
	f.gone = nil
	for _, p := range ps {
		f.rows[p.ID] = p
		f.order = append(f.order, p.ID)
	}
}

// row returns the stored Persisted for id, for a test to inspect what
// persistIfChanged actually wrote.
func (f *fakeStore) row(id string) (Persisted, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[id]
	return p, ok
}

func (f *fakeStore) Load(context.Context) ([]Persisted, error) {
	if f.loadHook != nil {
		f.loadHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	out := make([]Persisted, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.rows[id])
	}
	return out, nil
}

func (f *fakeStore) Save(_ context.Context, p Persisted) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.rows == nil {
		f.rows = map[string]Persisted{}
	}
	if _, ok := f.rows[p.ID]; !ok {
		f.order = append(f.order, p.ID)
	}
	f.rows[p.ID] = p
	// Saving a row un-deletes it. Without this, deleted() keeps reporting true
	// for an ID that Load now returns, so a test covering delete-then-re-add
	// would be asserting against a contradiction the real store cannot produce.
	delete(f.gone, p.ID)
	f.saves++
	return nil
}

// saveCount reports how many times Save was called. Tests use it to assert a
// NEGATIVE — that a quiet tick wrote nothing — which row() cannot express,
// since a Save that rewrites identical content leaves no trace in rows.
func (f *fakeStore) saveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saves
}

func (f *fakeStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	if f.gone == nil {
		f.gone = map[string]bool{}
	}
	f.gone[id] = true
	delete(f.rows, id)
	f.order = slices.DeleteFunc(f.order, func(s string) bool { return s == id })
	return nil
}

func (f *fakeStore) deleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gone[id]
}

// fakeRunner is the Runner test double.
type fakeRunner struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (f *fakeRunner) Run(_ context.Context, id string, _ job.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	f.seen[id] = true
}

func (f *fakeRunner) started(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[id]
}

// testOpts is how every test in this package varies the dispatcher it builds.
// One constructor with options, rather than a family of
// newTestDispatcherWithX helpers: later tasks need to vary four things
// independently, and the combinatorial helper set is the thing that rots.
type testOpts struct {
	leaseCap, slotCap int
	res               Residency
	store             Store
	runner            Runner
	workers           sched.Workers
}

func newTestDispatcher(t *testing.T, mods ...func(*testOpts)) *Dispatcher {
	t.Helper()
	o := testOpts{
		leaseCap: 2, slotCap: 2,
		res:     &fakeResidency{},
		store:   &fakeStore{},
		runner:  &fakeRunner{},
		workers: &stubWorkers{},
	}
	for _, m := range mods {
		m(&o)
	}
	// Built through New, not by a second struct literal. Repeating New's
	// initialisation here made a second constructor for one type — the first
	// smell Standing Design Rule 2 names — and it silently diverges the
	// moment New gains a field or an invariant, which is exactly the failure
	// a test helper cannot afford. tickEvery is an hour so that only explicit
	// d.tick calls advance anything.
	//
	// The log is the one deliberate override: New leaves slog.Default, and a
	// test that exercises error paths would otherwise print them.
	d := New(o.leaseCap, o.slotCap, time.Hour, testClock, o.workers, o.res, o.store, o.runner)
	d.log = slog.New(slog.DiscardHandler)
	return d
}

// The with* options each vary one axis of the test dispatcher, so every
// task's tests compile against one constructor rather than each task adding
// its own. All five have callers now: grepping each name across
// internal/dispatch/*_test.go and discounting its own declaration finds
// withCaps 3, withResidency 7, withStore 17, withRunner 4, withWorkers 1.
func withCaps(lease, slot int) func(*testOpts) {
	return func(o *testOpts) { o.leaseCap, o.slotCap = lease, slot }
}

func withResidency(r Residency) func(*testOpts) { return func(o *testOpts) { o.res = r } }

func withStore(s Store) func(*testOpts) { return func(o *testOpts) { o.store = s } }

func withRunner(r Runner) func(*testOpts) { return func(o *testOpts) { o.runner = r } }

func withWorkers(w sched.Workers) func(*testOpts) { return func(o *testOpts) { o.workers = w } }
