package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// The three ports the dispatcher needs that this test does not exercise. They
// are inert on purpose: the subject here is the Store, and a Runner that
// actually launched work would make the assertions depend on scheduling.
type nopWorkers struct{}

func (nopWorkers) Abort(string) {}

type nopResidency struct{}

func (nopResidency) Hydrate(context.Context, string) error { return nil }
func (nopResidency) Evict(string)                          {}

type nopRunner struct{}

func (nopRunner) Run(context.Context, string, job.State) {}

// tickEvery is short because these tests drive the dispatcher through its
// EXPORTED surface, and persistIfChanged only runs on a tick. internal/dispatch's
// own tests call the unexported tick directly and so can use an hour; from
// outside the package the loop has to actually run.
const tickEvery = 5 * time.Millisecond

// waitFor polls the store until pred holds, or fails. It polls rather than
// sleeping a fixed interval: a fixed sleep either flakes under parallel load or
// wastes the difference on every run, and the deadline is generous enough that
// only a genuine failure to persist reaches it.
func waitFor(t *testing.T, s *store.Store, what string, pred func([]dispatch.Persisted) bool) []dispatch.Persisted {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []dispatch.Persisted
	for time.Now().Before(deadline) {
		rows, err := s.Load(t.Context())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		last = rows
		if pred(rows) {
			return rows
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; store holds %d rows: %+v", what, len(last), last)
	return nil
}

func rowCount(n int) func([]dispatch.Persisted) bool {
	return func(rows []dispatch.Persisted) bool { return len(rows) == n }
}

// wantJob is one expected queue entry. It is a named type rather than an
// anonymous struct so the eviction below can filter the slice.
type wantJob struct {
	id  string
	hdr dispatch.Header
	pol job.Policy
}

// This file holds the only tests that run a Dispatcher against a real database.
// Every test in internal/dispatch uses an in-memory fakeStore, so nothing
// outside this file exercises driver behaviour — type coercion, scan errors,
// real constraint violations — from the dispatcher's side.
//
// TestDispatcher_RoundTripsThroughRealSQLite asserts the property B2.4 will
// depend on: a queue persisted by one Dispatcher comes back from a second one
// with its order, headers, policies and axes intact. Policy is checked through
// the store rather than through List, because Row carries a Header and a
// RenderView and deliberately does not expose Policy.
func TestDispatcher_RoundTripsThroughRealSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(history.NewRepository(db).DB())

	newDispatcher := func() *dispatch.Dispatcher {
		return dispatch.New(2, 2, tickEvery, time.Now,
			nopWorkers{}, nopResidency{}, st, nopRunner{})
	}

	pol := job.Policy{Verify: true, Repair: true, Unpack: true, Delete: true}
	now := time.Now().UTC().Unix()
	want := []wantJob{
		{"first", dispatch.Header{Name: "first", Category: "tv", Priority: 1, Bytes: 100, Added: now}, pol},
		{"second", dispatch.Header{Name: "second", Category: "movies", Priority: 2, Bytes: 200, Added: now}, job.Policy{}},
		{"third", dispatch.Header{Name: "third", Bytes: 300, Added: now}, job.Policy{Verify: true}},
	}

	d1 := newDispatcher()
	if err := d1.Start(t.Context()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	for _, w := range want {
		if err := d1.Add(job.New(w.id, w.hdr.Name, w.pol), w.hdr); err != nil {
			t.Fatalf("Add(%s): %v", w.id, err)
		}
	}
	// Cancelling a never-run job EVICTS it — from residency, the registry and
	// the store — rather than parking it with IntentCancel. That is B2.3's
	// documented behaviour (the RFC's carried-forward item 4: a cancelled
	// never-run job would otherwise render as Queued forever), and asserting
	// it here is what puts Store.Delete on a real driver rather than only on
	// the fake.
	waitFor(t, st, "all three jobs persisted", rowCount(len(want)))

	// Cancel one so a non-zero Intent has to survive the round trip. A queue
	// where every axis holds its zero value would round-trip cleanly through a
	// completely broken column mapping, which is worth nothing.
	//
	// This does NOT evict: eviction is for a cancelled job that has never run,
	// and by now the tick has opened an attempt on all three. The eviction path
	// is covered by TestStore_DeleteRemovesARow against the same driver.
	if err := d1.Cancel("second"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitFor(t, st, "the cancel to reach the database", func(rows []dispatch.Persisted) bool {
		for _, r := range rows {
			if r.ID == "second" {
				return r.Intent == job.IntentCancel
			}
		}
		return false
	})
	if err := d1.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Everything below runs against a second Dispatcher that has never seen
	// those jobs in memory: whatever it reports came off disk.
	d2 := newDispatcher()
	if err := d2.Start(t.Context()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	t.Cleanup(func() { _ = d2.Stop() })

	rows := d2.List()
	if len(rows) != len(want) {
		t.Fatalf("restored %d jobs, want %d", len(rows), len(want))
	}
	for _, r := range rows {
		if r.ID == "second" && r.View.Intent != job.IntentCancel {
			t.Errorf("second came back with Intent %s, want IntentCancel — the cancel did not survive the round trip", r.View.Intent)
		}
	}

	// Policy is not on Row, so it is asserted against the store. Without this
	// the comment above claims more than the test checks: three jobs with
	// three different policies would round-trip identically through a Policy
	// column that was never written.
	stored, err := st.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byID := make(map[string]dispatch.Persisted, len(stored))
	for _, p := range stored {
		byID[p.ID] = p
	}
	for _, w := range want {
		p, ok := byID[w.id]
		if !ok {
			t.Errorf("%s is not in the store after the round trip", w.id)
			continue
		}
		if p.Policy != w.pol {
			t.Errorf("%s Policy = %v, want %v", w.id, p.Policy, w.pol)
		}
	}
	for i, w := range want {
		if rows[i].ID != w.id {
			t.Errorf("row %d is %s, want %s — queue order did not survive the round trip", i, rows[i].ID, w.id)
		}
		if rows[i].Header != w.hdr {
			t.Errorf("%s header = %+v, want %+v", w.id, rows[i].Header, w.hdr)
		}
	}
}

// TestDispatcher_QuietRestartWritesNothing pins, against a real database, that
// restoring a queue and ticking it does not rewrite the rows it just read.
//
// internal/dispatch has an equivalent test against its fake. This one is worth
// its own seat because the failure it guards is invisible for one restart: the
// first restart renumbers, and only the SECOND comes back in the wrong order.
// A test that stops after one round trip cannot see it.
func TestDispatcher_QuietRestartWritesNothing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(history.NewRepository(db).DB())

	newDispatcher := func() *dispatch.Dispatcher {
		return dispatch.New(2, 2, tickEvery, time.Now,
			nopWorkers{}, nopResidency{}, st, nopRunner{})
	}
	ids := []string{"a", "b", "c"}

	d1 := newDispatcher()
	if err := d1.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, id := range ids {
		if err := d1.Add(job.New(id, id, job.Policy{}), dispatch.Header{Name: id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}
	afterFirst := waitFor(t, st, "the initial queue to persist", rowCount(len(ids)))
	if err := d1.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Two more full restarts. If restore and persistIfChanged disagree about
	// what is stored, the keys drift on each one.
	//
	// Each restart adds a probe job and waits for IT to appear. Waiting on the
	// original row count would be vacuous: those rows are already on disk
	// before Start is called, so the predicate holds on the first poll and the
	// test would stop the dispatcher without a single tick having run — proving
	// nothing about what a tick does to restored rows. The probe only appears
	// once persistIfChanged has actually executed.
	for i := range 2 {
		probe := fmt.Sprintf("probe%d", i)
		d := newDispatcher()
		if err := d.Start(t.Context()); err != nil {
			t.Fatalf("restart Start: %v", err)
		}
		if err := d.Add(job.New(probe, probe, job.Policy{}), dispatch.Header{Name: probe}); err != nil {
			t.Fatalf("Add(%s): %v", probe, err)
		}
		waitFor(t, st, "the probe job to persist, proving a tick ran", func(rows []dispatch.Persisted) bool {
			return slices.ContainsFunc(rows, func(p dispatch.Persisted) bool { return p.ID == probe })
		})
		if err := d.Stop(); err != nil {
			t.Fatalf("restart Stop: %v", err)
		}
	}

	all, err := st.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Compare only the original jobs; the probes are scaffolding.
	afterRestarts := slices.DeleteFunc(all, func(p dispatch.Persisted) bool {
		return !slices.Contains(ids, p.ID)
	})
	if len(afterRestarts) != len(afterFirst) {
		t.Fatalf("row count for the original jobs changed across restarts: %d -> %d", len(afterFirst), len(afterRestarts))
	}
	for i := range afterFirst {
		if afterFirst[i] != afterRestarts[i] {
			t.Errorf("row %d changed across two quiet restarts:\n before %+v\n  after %+v",
				i, afterFirst[i], afterRestarts[i])
		}
	}
}
