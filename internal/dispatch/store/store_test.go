package store_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// newTestStore opens a real migrated database. The point of this package's
// tests is the driver's behaviour — type coercion, scan errors, the actual
// schema — so an in-memory double would test nothing that dispatch's own
// fakeStore does not already cover.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, _ := newTestStoreDB(t)
	return s
}

// newTestStoreDB also hands back the raw handle, for the one test that has to
// write a row this package could not have written.
func newTestStoreDB(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	db, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw := history.NewRepository(db).DB()
	return store.New(raw), raw
}

// TestStore_RoundTripsEveryAxis uses an all-true Policy deliberately. A mixed
// literal leaves its false fields agreeing with the column DEFAULT 0, so a
// swapped or unmapped boolean column round-trips correctly by accident.
func TestStore_RoundTripsEveryAxis(t *testing.T) {
	s := newTestStore(t)
	want := dispatch.Persisted{
		ID:      "j1",
		SortKey: 7,
		Header:  dispatch.Header{Name: "n", Category: "tv", Priority: 2, Bytes: 1 << 20},
		Policy:  job.Policy{Verify: true, Repair: true, Unpack: true, Delete: true},
		State: job.StateView{
			State: job.Repairing, Next: job.Extracting,
			Activity: job.ActPar2Repair, Outcome: job.OutcomePending, Assessed: true,
		},
		Intent: job.IntentCancel,
	}
	if err := s.Save(t.Context(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Load returned %d rows, want 1", len(got))
	}
	if got[0] != want {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got[0], want)
	}
}

// TestStore_RoundTripsEveryPolicyCombination is what makes a swapped boolean
// column detectable: with all sixteen combinations, no single mapping error
// can survive.
func TestStore_RoundTripsEveryPolicyCombination(t *testing.T) {
	s := newTestStore(t)
	for i := range 16 {
		want := job.Policy{
			Verify: i&1 != 0, Repair: i&2 != 0,
			Unpack: i&4 != 0, Delete: i&8 != 0,
		}
		p := dispatch.Persisted{ID: "j", Header: dispatch.Header{Name: "n"}, Policy: want}
		if err := s.Save(t.Context(), p); err != nil {
			t.Fatalf("Save(%+v): %v", want, err)
		}
		got, err := s.Load(t.Context())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(got) != 1 || got[0].Policy != want {
			t.Fatalf("Policy round trip: got %+v, want %+v", got, want)
		}
	}
}

// TestStore_RoundTripsEveryEnumMember fails when a new State, Outcome, Intent
// or Activity is declared and not mapped, rather than letting it persist
// silently as its zero value.
func TestStore_RoundTripsEveryEnumMember(t *testing.T) {
	s := newTestStore(t)
	save := func(t *testing.T, p dispatch.Persisted) dispatch.Persisted {
		t.Helper()
		if err := s.Save(t.Context(), p); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := s.Load(t.Context())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("Load returned %d rows, want 1", len(got))
		}
		return got[0]
	}
	base := dispatch.Persisted{ID: "j", Header: dispatch.Header{Name: "n"}}

	for _, st := range job.AllStates() {
		p := base
		p.State = job.StateView{State: st}
		if got := save(t, p); got.State.State != st {
			t.Errorf("State %s round-tripped as %s", st, got.State.State)
		}
	}
	for _, st := range job.AllStates() {
		p := base
		p.State = job.StateView{State: job.Fetching, Next: st}
		if got := save(t, p); got.State.Next != st {
			t.Errorf("Next %s round-tripped as %s", st, got.State.Next)
		}
	}
	for _, o := range job.AllOutcomes() {
		p := base
		p.State = job.StateView{State: job.Fetching, Outcome: o}
		if got := save(t, p); got.State.Outcome != o {
			t.Errorf("Outcome %s round-tripped as %s", o, got.State.Outcome)
		}
	}
	for _, in := range job.AllIntents() {
		p := base
		p.Intent = in
		if got := save(t, p); got.Intent != in {
			t.Errorf("Intent %s round-tripped as %s", in, got.Intent)
		}
	}
	for _, a := range job.AllActivities() {
		p := base
		p.State = job.StateView{State: job.Fetching, Activity: a}
		if got := save(t, p); got.State.Activity != a {
			t.Errorf("Activity %s round-tripped as %s", a, got.State.Activity)
		}
	}
}

func TestStore_LoadOrdersBySortKeyThenID(t *testing.T) {
	s := newTestStore(t)
	// b and c collide on SortKey, so the ID tiebreak is what makes the result
	// total rather than whatever order SQLite happens to return.
	for _, p := range []dispatch.Persisted{
		{ID: "z", SortKey: 5, Header: dispatch.Header{Name: "z"}},
		{ID: "c", SortKey: 1, Header: dispatch.Header{Name: "c"}},
		{ID: "b", SortKey: 1, Header: dispatch.Header{Name: "b"}},
	} {
		if err := s.Save(t.Context(), p); err != nil {
			t.Fatalf("Save(%s): %v", p.ID, err)
		}
	}
	got, err := s.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if want := []string{"b", "c", "z"}; !slices.Equal(ids, want) {
		t.Fatalf("Load order = %v, want %v", ids, want)
	}
}

// TestStore_SaveUpdatesAnExistingRow covers the upsert arm. A round-trip of a
// single fresh row exercises only the INSERT.
func TestStore_SaveUpdatesAnExistingRow(t *testing.T) {
	s := newTestStore(t)
	first := dispatch.Persisted{
		ID: "j1", SortKey: 3,
		Header: dispatch.Header{Name: "before", Category: "a", Priority: 1, Bytes: 10},
		State:  job.StateView{State: job.Fetching},
	}
	if err := s.Save(t.Context(), first); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	second := dispatch.Persisted{
		ID: "j1", SortKey: 9,
		Header: dispatch.Header{Name: "after", Category: "b", Priority: 2, Bytes: 20},
		Policy: job.Policy{Verify: true, Repair: true, Unpack: true, Delete: true},
		State: job.StateView{
			State: job.Finalizing, Activity: job.ActMove,
			Outcome: job.OutcomeOK, Assessed: true,
		},
		Intent: job.IntentPause,
	}
	if err := s.Save(t.Context(), second); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	got, err := s.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Load returned %d rows after re-Saving one ID, want 1", len(got))
	}
	if got[0] != second {
		t.Fatalf("upsert did not replace every column:\n got %+v\nwant %+v", got[0], second)
	}
}

func TestStore_DeleteRemovesARow(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"a", "b"} {
		if err := s.Save(t.Context(), dispatch.Persisted{ID: id, Header: dispatch.Header{Name: id}}); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}
	if err := s.Delete(t.Context(), "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("after deleting a, Load = %+v, want only b", got)
	}
}

// TestStore_DeleteAnAbsentRowIsNotAnError is load-bearing rather than a
// tidiness check. evictCancelledNeverRun treats the job's pass as over whether
// or not the delete succeeded, so making absence an error would turn a
// redundant evict into a logged failure on every subsequent tick.
func TestStore_DeleteAnAbsentRowIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete(t.Context(), "never-existed"); err != nil {
		t.Fatalf("Delete of an absent row = %v, want nil", err)
	}
}

// TestStore_LoadRejectsAnOutOfRangeEnum pins that a value too large for the
// uint8 the job enums are built on fails the load rather than truncating.
//
// Truncation is the dangerous outcome, not merely a wrong one: 256 narrows to
// 0, which is StateUnset — a LEGAL persisted position meaning "this job never
// ran". A corrupt row would restore as a plausible queued job, and reconstruct
// could not object, because StateUnset is exactly the shape it accepts without
// opening an attempt.
func TestStore_LoadRejectsAnOutOfRangeEnum(t *testing.T) {
	s, raw := newTestStoreDB(t)
	if err := s.Save(t.Context(), dispatch.Persisted{ID: "j1", Header: dispatch.Header{Name: "n"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 256 is chosen over an arbitrary large number because it is the exact
	// value that truncates to StateUnset.
	if _, err := raw.ExecContext(t.Context(), `UPDATE dispatch_jobs SET state = 256 WHERE id = ?`, "j1"); err != nil {
		t.Fatalf("corrupting the row: %v", err)
	}
	got, err := s.Load(t.Context())
	if err == nil {
		t.Fatalf("Load returned %+v and no error, want an error — 256 truncates to StateUnset, which reads as a legal never-run job", got)
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("Load error = %v, want it to name the out-of-range value", err)
	}
}
