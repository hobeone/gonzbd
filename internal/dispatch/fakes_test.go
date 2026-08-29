package dispatch

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/sched"
)

type stubWorkers struct{ aborted []string }

func (s *stubWorkers) Abort(jobID string) { s.aborted = append(s.aborted, jobID) }

func testClock() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// fakeResidency is the Residency test double. failOn lets a test force
// Hydrate to fail for one job ID, for Task 4's hydration-failure test.
type fakeResidency struct {
	mu     sync.Mutex
	live   map[string]bool
	failOn map[string]error
}

func (f *fakeResidency) Hydrate(_ context.Context, id string) error {
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
	mu   sync.Mutex
	rows map[string]Persisted
	gone map[string]bool
}

func (f *fakeStore) Load(context.Context) ([]Persisted, error) { return nil, nil }

func (f *fakeStore) Save(_ context.Context, p Persisted) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string]Persisted{}
	}
	f.rows[p.ID] = p
	return nil
}

func (f *fakeStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone == nil {
		f.gone = map[string]bool{}
	}
	f.gone[id] = true
	delete(f.rows, id)
	return nil
}

//nolint:unused // first caller is Task 6's eviction tests
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

//nolint:unused // first caller is Task 5's launch tests
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
	return &Dispatcher{
		byID:      map[string]*entry{},
		resident:  map[string]bool{},
		launched:  map[string]bool{},
		written:   map[string]Persisted{},
		q:         sched.New(o.leaseCap, o.slotCap, testClock, o.workers),
		wake:      make(chan struct{}, 1),
		res:       o.res,
		store:     o.store,
		runner:    o.runner,
		tickEvery: time.Hour, // long, so only explicit d.tick calls advance anything
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		log:       slog.New(slog.DiscardHandler),
	}
}

// The with* options are each unused until the task whose tests vary that
// axis: withCaps (Task 3, lease-contention tests), withResidency (Task 4),
// withStore (Task 6), withRunner (Task 5), withWorkers (Task 5's abort-loop
// test). Declaring all five now, per the ruling in
// .superpowers/sdd/2026-08-28-sched-dispatcher/progress.md, means every later
// task's tests compile against one constructor rather than each task adding
// its own.

//nolint:unused // first caller is Task 3
func withCaps(lease, slot int) func(*testOpts) {
	return func(o *testOpts) { o.leaseCap, o.slotCap = lease, slot }
}

func withResidency(r Residency) func(*testOpts) { return func(o *testOpts) { o.res = r } }

//nolint:unused // first caller is Task 6
func withStore(s Store) func(*testOpts) { return func(o *testOpts) { o.store = s } }

//nolint:unused // first caller is Task 5
func withRunner(r Runner) func(*testOpts) { return func(o *testOpts) { o.runner = r } }

//nolint:unused // first caller is Task 5's abort-loop test
func withWorkers(w sched.Workers) func(*testOpts) { return func(o *testOpts) { o.workers = w } }
