package dispatch

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestRestore_RegistersEveryStoredJobInOrder pins that restore preserves the
// store's row order in the registry (D-B11's read-once-at-startup), rather
// than reordering by ID or by any other key.
// TestRestore_RegistersEveryStoredJobInOrder asserts restore rebuilds queue
// order from SortKey, not from the order Load happened to return rows in.
//
// The seeded rows are deliberately out of both orders at once: "c" sorts after
// "a" alphabetically and is returned FIRST by Load, yet carries the lower key,
// so only a restore that reads SortKey produces [c a]. Before B2.2 this test
// seeded no keys and passed on Load's slice order alone, which is the premise
// this change replaced.
func TestRestore_RegistersEveryStoredJobInOrder(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{
		{ID: "c", SortKey: 1, Header: Header{Name: "c"}},
		{ID: "a", SortKey: 2, Header: Header{Name: "a"}},
	})
	d := newTestDispatcher(t, withStore(st))

	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got := d.List()
	want := []string{"c", "a"}
	if len(got) != len(want) {
		t.Fatalf("List has %d rows after restore, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("row %d is %s, want %s — restore must rebuild queue order from SortKey", i, got[i].ID, want[i])
		}
	}
}

// TestRestore_JobsComeBackHoldingNothing pins D-B13's Startup paragraph: the
// pools are process-local, so a job persisted mid-Repairing comes back at
// Repairing holding no lease — there is nothing in this process to reclaim.
//
// Assessed is set true here because it is the only value a real Repairing row
// could carry: Repairing's one inbound edge is Assessing (legalEdges,
// internal/job/transition.go:47), and Assessed is set unconditionally on
// entering Assessing (transition, internal/job/attempt.go:280-281) with no
// door that later clears it — so a Repairing row with Assessed false names a
// position no attempt could have reached. The fixture does not trust this
// field at face value; reconstruct re-derives it by replaying the job through
// the same doors a live job would have crossed, and a restored row's Assessed
// is correct because of that replay, not because the fixture asserts it.
func TestRestore_JobsComeBackHoldingNothing(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{{
		ID:     "j1",
		Header: Header{Name: "j1"},
		State:  job.StateView{State: job.Repairing, Assessed: true},
	}})
	d := newTestDispatcher(t, withStore(st))

	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	j, ok := d.lookup("j1")
	if !ok {
		t.Fatal("job was not registered")
	}
	if j.HoldsLease() {
		t.Error("restored job holds a lease — the pools are process-local and there is nothing to reclaim, so the first tick must re-acquire through grantFor")
	}
}

// TestRestore_StoreErrorFailsStart pins that a store read failure at Start
// fails Start outright rather than silently starting with a partial queue.
func TestRestore_StoreErrorFailsStart(t *testing.T) {
	st := &fakeStore{loadErr: errors.New("database is locked")}
	d := newTestDispatcher(t, withStore(st))

	if err := d.Start(context.Background()); err == nil {
		t.Fatal("Start returned nil after a store failure, want an error — starting with a partial queue would silently drop jobs")
	}
}

// TestPersist_WritesWhenTheAxesMove pins that a job's first BeginAttempt —
// State moving off StateUnset — is written to the store by the end of the
// tick that produced it, so a restart does not re-run the job from scratch.
func TestPersist_WritesWhenTheAxesMove(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background()) // BeginAttempt moves State off StateUnset

	got, ok := st.row("j1")
	if !ok || got.State.State == job.StateUnset {
		t.Errorf("stored State = %+v (present=%v), want a started state — a restart would otherwise re-run the job from the beginning", got.State, ok)
	}
}

// TestPersist_QuietTickWritesNothing pins the "no store traffic on a quiet
// tick" half of persistIfChanged: once a job's row matches what was last
// written, a further tick over the same, unchanged job must not call Save
// again. The job here is at Fetching after the first tick (BeginAttempt only
// — it has not yet been granted a lease). The second, "quiet" tick DOES grant
// it one, but that moves what the job HOLDS, not its StateView: State stays
// Fetching with Next/Activity/Outcome untouched, so the Persisted row
// persistIfChanged builds is unchanged and the comparison against d.written
// is what actually suppresses the write — not the job being idle.
func TestPersist_QuietTickWritesNothing(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background())
	first, ok := st.row("j1")
	if !ok {
		t.Fatal("row not written on the first tick")
	}

	// Overwrite the stored row with a sentinel Header the dispatcher never
	// supplies, so a second unwanted Save is observable: persistIfChanged
	// comparing against d.written (in memory) rather than the store will
	// leave this sentinel in place if it correctly skips the write.
	st.mu.Lock()
	sentinel := first
	sentinel.Header.Name = "sentinel-should-survive"
	st.rows["j1"] = sentinel
	st.mu.Unlock()

	d.tick(context.Background())

	got, _ := st.row("j1")
	if got.Header.Name != "sentinel-should-survive" {
		t.Errorf("row was overwritten on a quiet tick (Header.Name = %q), want persistIfChanged to skip an unchanged job", got.Header.Name)
	}
}

// TestRestore_PostBoundaryJobConsumesTheBoundary pins that a job restored
// past the irreversible boundary — reconstruct reaches Finalizing only
// through Cross — reports ErrBoundaryConsumed to a later Retry, exactly as a
// job that crossed within this process would. This is reconstruct's third
// stated advantage over a second constructor: boundary consumption falls out
// of using Cross rather than being special-cased.
func TestRestore_PostBoundaryJobConsumesTheBoundary(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{{
		ID:     "j1",
		Header: Header{Name: "j1"},
		State:  job.StateView{State: job.Finalizing, Outcome: job.OutcomeOK},
	}})
	d := newTestDispatcher(t, withStore(st))

	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	err := d.Retry("j1")
	if !errors.Is(err, job.ErrBoundaryConsumed) {
		t.Errorf("Retry() = %v, want ErrBoundaryConsumed — a job restored at Finalizing really did cross, and a fresh attempt is not a legal way to retry it", err)
	}
}

// TestRestore_RejectsAnUnreachablePosition pins that restore's replay is a
// real validation, not a trusted deserialization: a row claiming OutcomeOK
// at Fetching (admissibleAt allows OutcomeOK only at Finalizing —
// internal/job/admissibility.go) is not a legal sequence job.Job's doors will
// produce, and restore must refuse it rather than register a job in a
// position the state machine itself calls invalid.
func TestRestore_RejectsAnUnreachablePosition(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{{
		ID:     "j1",
		Header: Header{Name: "j1"},
		State:  job.StateView{State: job.Fetching, Outcome: job.OutcomeOK},
	}})
	d := newTestDispatcher(t, withStore(st))

	err := d.restore(context.Background())
	if err == nil {
		t.Fatal("restore() = nil, want an error — OutcomeOK is not admissible at Fetching")
	}
	if !errors.Is(err, job.ErrInvalidOutcome) {
		t.Errorf("restore() = %v, want it to wrap job.ErrInvalidOutcome", err)
	}
}

// TestRestore_ReachesExtractingViaCross is a narrower cousin of the
// boundary-consumption test above: it pins that Extracting itself (not just
// Finalizing) replays cleanly, since it is the state Cross lands on directly
// and Finalizing only reaches it by continuing one hop further.
func TestRestore_ReachesExtractingViaCross(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{{
		ID:     "j1",
		Header: Header{Name: "j1"},
		State:  job.StateView{State: job.Extracting},
	}})
	d := newTestDispatcher(t, withStore(st))

	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	j, ok := d.lookup("j1")
	if !ok {
		t.Fatal("job was not registered")
	}
	if got := j.State().State; got != job.Extracting {
		t.Errorf("State() = %s, want Extracting", got)
	}
}

// TestReconstruct_NeverRunRoundTrips is reconstruct's direct-call test for
// the StateUnset branch: a never-run row produces a job with no attempt and
// the persisted Intent carried over.
func TestReconstruct_NeverRunRoundTrips(t *testing.T) {
	j, err := reconstruct("j1", "n", job.Policy{}, job.StateView{}, job.IntentPause, testClock())
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if j.HasRun() {
		t.Error("HasRun() = true for a StateUnset row, want false — nothing should have opened an attempt")
	}
	if got := j.Intent(); got != job.IntentPause {
		t.Errorf("Intent() = %s, want IntentPause", got)
	}
}

// TestReconstruct_NeverRunRejectsAStrayNext pins the StateUnset guard: a row
// claiming State == StateUnset but carrying a recorded Next names a position
// no never-run attempt could hold, since Next is only ever written onto an
// OPEN attempt (job.Job.SetNext).
func TestReconstruct_NeverRunRejectsAStrayNext(t *testing.T) {
	v := job.StateView{State: job.StateUnset, Next: job.Assessing}
	if _, err := reconstruct("j1", "n", job.Policy{}, v, job.IntentRun, testClock()); err == nil {
		t.Fatal("reconstruct() = nil error, want one — StateUnset with Next set is not a reachable row")
	}
}

// TestReconstruct_FetchingWithAssessedRoundTrips pins the one case where
// v.Assessed genuinely steers the replay: a job that has already been
// through Assessing and returned to Fetching (e.g. after a Repairing
// verdict) must come back reporting Assessed true, not merely "at Fetching".
func TestReconstruct_FetchingWithAssessedRoundTrips(t *testing.T) {
	v := job.StateView{State: job.Fetching, Assessed: true}
	j, err := reconstruct("j1", "n", job.Policy{}, v, job.IntentRun, testClock())
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	got := j.State()
	if got.State != job.Fetching {
		t.Errorf("State = %s, want Fetching", got.State)
	}
	if !got.Assessed {
		t.Error("Assessed = false, want true — the replay must round-trip through Assessing for a row that says it already has")
	}
}

// TestReconstruct_AssessingRoundTrips covers reconstruct's Assessing case
// directly, including a recorded Next (the generic SetNext step after the
// switch) and a recorded Activity (the generic SetActivity step) — both
// legal at Assessing, both exercised by no other test in this file.
func TestReconstruct_AssessingRoundTrips(t *testing.T) {
	v := job.StateView{State: job.Assessing, Next: job.Repairing, Activity: job.ActPar2Verify}
	j, err := reconstruct("j1", "n", job.Policy{}, v, job.IntentRun, testClock())
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	got := j.State()
	if got.State != job.Assessing || got.Next != job.Repairing || got.Activity != job.ActPar2Verify {
		t.Errorf("State() = %+v, want State=Assessing Next=Repairing Activity=Par2Verify", got)
	}
}

// TestReconstruct_RejectsAnIllegalNext pins the generic SetNext step's own
// validation: Next must be a legal edge FROM v.State (SetNext's
// CanTransition check), not merely a declared State. Fetching's only edge is
// to Assessing, so a Next of Repairing here is unreachable.
func TestReconstruct_RejectsAnIllegalNext(t *testing.T) {
	v := job.StateView{State: job.Fetching, Next: job.Repairing}
	if _, err := reconstruct("j1", "n", job.Policy{}, v, job.IntentRun, testClock()); err == nil {
		t.Fatal("reconstruct() = nil error, want one — Fetching has no edge to Repairing")
	}
}

// TestReconstruct_RejectsAnUndeclaredState pins the switch's default branch:
// a State value AllStates() does not declare is refused rather than treated
// as any of the five real positions.
func TestReconstruct_RejectsAnUndeclaredState(t *testing.T) {
	v := job.StateView{State: job.State(99)}
	if _, err := reconstruct("j1", "n", job.Policy{}, v, job.IntentRun, testClock()); err == nil {
		t.Fatal("reconstruct() = nil error, want one — State(99) is not declared")
	}
}

// TestReconstruct_RejectsAnInvalidIntent pins the final SetIntent call's
// error path: an Intent value AllIntents() does not declare is refused
// rather than silently stored.
func TestReconstruct_RejectsAnInvalidIntent(t *testing.T) {
	v := job.StateView{State: job.Fetching}
	if _, err := reconstruct("j1", "n", job.Policy{}, v, job.Intent(99), testClock()); err == nil {
		t.Fatal("reconstruct() = nil error, want one — Intent(99) is not declared")
	}
}

// TestTakeHop_TransitionAndCross is a direct-call test for reconstruct's
// hop primitive: a plain hop takes an ordinary Transition, and a cross hop
// records the destination with SetNext before taking Cross — the two-step
// dance Cross itself requires (it refuses to move without a recorded
// destination).
func TestTakeHop_TransitionAndCross(t *testing.T) {
	j := job.New("j1", "n", job.Policy{})
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}

	if err := takeHop(j, hop{job.Assessing, false}); err != nil {
		t.Fatalf("takeHop(Assessing, plain): %v", err)
	}
	if got := j.State().State; got != job.Assessing {
		t.Fatalf("State() = %s, want Assessing", got)
	}

	if err := takeHop(j, hop{job.Extracting, true}); err != nil {
		t.Fatalf("takeHop(Extracting, cross): %v", err)
	}
	got := j.State()
	if got.State != job.Extracting {
		t.Errorf("State() = %s, want Extracting", got.State)
	}
	if got.Next != job.StateUnset {
		t.Errorf("Next() = %s, want StateUnset — Cross clears the destination it consumed", got.Next)
	}
}

// TestReconstruct_NeverRunRejectsAnInvalidIntent covers the StateUnset
// branch's own SetIntent call, distinct from TestReconstruct_RejectsAnInvalidIntent
// above which exercises the same check after the switch/hop path instead.
func TestReconstruct_NeverRunRejectsAnInvalidIntent(t *testing.T) {
	if _, err := reconstruct("j1", "n", job.Policy{}, job.StateView{}, job.Intent(99), testClock()); err == nil {
		t.Fatal("reconstruct() = nil error, want one — Intent(99) is not declared")
	}
}

// TestPersistIfChanged_WritesHeaderFromTheRegistry calls persistIfChanged
// directly (rather than only through tick) so it is exercised as its own
// unit: the Persisted row it produces must carry the Header Add supplied,
// which Render alone cannot reconstruct.
func TestPersistIfChanged_WritesHeaderFromTheRegistry(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	h := Header{Name: "n", Category: "movies", Priority: 1, Bytes: 42}
	if err := d.Add(j, h); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.persistIfChanged(context.Background(), j)

	got, ok := st.row("j1")
	if !ok {
		t.Fatal("persistIfChanged did not write a row")
	}
	if got.Header != h {
		t.Errorf("Header = %+v, want %+v", got.Header, h)
	}
}

// TestPersistIfChanged_SkipsAnUnregisteredJob pins headerFor's guard inside
// persistIfChanged: a job with no registry entry (evicted, or never added)
// has nowhere to attach a Header, so persistIfChanged must not call Save for
// it.
func TestPersistIfChanged_SkipsAnUnregisteredJob(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("ghost", "n", job.Policy{})

	d.persistIfChanged(context.Background(), j)

	if _, ok := st.row("ghost"); ok {
		t.Error("persistIfChanged wrote a row for a job with no registry entry")
	}
}

// TestLastWrittenAndMarkWritten_RoundTrip is a direct-call test for the two
// small d.written accessors persistIfChanged composes: markWritten must make
// an immediately following lastWritten see exactly what was written, and an
// ID that was never written must report ok == false.
func TestLastWrittenAndMarkWritten_RoundTrip(t *testing.T) {
	d := newTestDispatcher(t)

	if _, ok := d.lastWritten("j1"); ok {
		t.Error("lastWritten(j1) = ok before any write, want false")
	}

	p := Persisted{ID: "j1", Header: Header{Name: "n"}, State: job.StateView{State: job.Fetching}}
	d.markWritten(p)

	got, ok := d.lastWritten("j1")
	if !ok {
		t.Fatal("lastWritten(j1) = not ok after markWritten, want ok")
	}
	if got != p {
		t.Errorf("lastWritten(j1) = %+v, want %+v", got, p)
	}
}

// TestHeaderFor_RoundTrip is a direct-call test for the registry lookup
// persistIfChanged uses to attach a Header to a Persisted row.
func TestHeaderFor_RoundTrip(t *testing.T) {
	d := newTestDispatcher(t)
	h := Header{Name: "n", Category: "tv"}
	if err := d.Add(job.New("j1", "n", job.Policy{}), h); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := d.headerFor("j1")
	if !ok || got != h {
		t.Errorf("headerFor(j1) = (%+v, %v), want (%+v, true)", got, ok, h)
	}

	if _, ok := d.headerFor("nope"); ok {
		t.Error("headerFor(nope) = ok, want false for an unregistered ID")
	}
}

// TestFakeStore_RowsAndOrderStayInStep pins the test double itself. Load
// indexes rows by each ID in order, so the two must move together: an ID left
// in order after Delete removed its row yields a zero-valued Persisted, and a
// Save that never reached order is invisible to Load. Both are shapes no real
// store can produce, so a test built on them would be exercising the fake.
func TestFakeStore_RowsAndOrderStayInStep(t *testing.T) {
	ctx := context.Background()
	f := &fakeStore{}
	f.seed([]Persisted{{ID: "a"}, {ID: "b"}})

	if err := f.Save(ctx, Persisted{ID: "c"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := f.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := f.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ids := make([]string, 0, len(got))
	for _, p := range got {
		if p.ID == "" {
			t.Errorf("Load returned a zero-valued row among %+v — an ID left in "+
				"order after its row was deleted reads back as an empty Persisted", got)
		}
		ids = append(ids, p.ID)
	}
	if want := []string{"b", "c"}; !slices.Equal(ids, want) {
		t.Errorf("Load returned %v, want %v — a saved row must become visible "+
			"to Load and a deleted one must disappear from it", ids, want)
	}

	// Re-seeding replaces the contents rather than layering over them.
	f.seed([]Persisted{{ID: "z"}})
	if _, ok := f.row("b"); ok {
		t.Error("row(b) survived a re-seed — seed replaces the store's contents")
	}
}

// TestRestore_RollsBackWhenALaterRowFails pins that a failed startup leaves
// the registry empty rather than half-populated.
//
// restore registers each row as it goes, so a row that fails partway leaves
// every earlier row in d.byID. Start reports the error and clears started, so
// the caller may legitimately retry once a transient store problem clears —
// but the retry re-Loads the same rows and d.Add refuses the first one with
// "already registered", so the dispatcher can never start again. In between,
// List and Stop operate on a queue that was never fully restored.
func TestRestore_RollsBackWhenALaterRowFails(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{
		{ID: "good", Header: Header{Name: "a"}, State: job.StateView{State: job.Fetching}},
		// StateUnset carrying a Next is a position no attempt can hold, so
		// reconstruct rejects it — a stand-in for any row that fails partway.
		{ID: "bad", Header: Header{Name: "b"}, State: job.StateView{State: job.StateUnset, Next: job.Assessing}},
	})
	d := newTestDispatcher(t, withStore(st))

	err := d.Start(context.Background())
	if err == nil {
		t.Fatal("Start() = nil, want an error — the second row cannot be reconstructed")
	}

	if got := len(d.List()); got != 0 {
		t.Errorf("registry holds %d jobs after a failed restore, want 0 — the "+
			"rows registered before the failure were left behind", got)
	}

	// The retry is the consequence that bites: same store, now readable.
	st.seed([]Persisted{
		{ID: "good", Header: Header{Name: "a"}, State: job.StateView{State: job.Fetching}},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v — a retry after a failed restore must be able "+
			"to register the rows the first attempt left behind", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	if got := len(d.List()); got != 1 {
		t.Errorf("registry holds %d jobs after the retry, want 1", got)
	}
}

// TestReconstruct_SettledRowsReplayAtEveryPosition pins that a settled row
// round-trips at every position an attempt can settle at.
//
// A review reading of reconstruct held that SetNext and SetActivity run
// unconditionally before Finish, and that SetNext's CanTransition check would
// therefore reject a settled row at a terminal position like Finalizing. Both
// calls are in fact guarded on a non-zero value, and Attempt.finish clears
// next and activity when it settles — so a settled row carries neither, the
// guards do not fire, and there is nothing for CanTransition to refuse. This
// test is that argument made executable rather than restated.
//
// OutcomeOK appears only for Finalizing because the admissibility table
// allows it nowhere else; the other positions settle Failed. That is the
// table working, not a replay limit.
func TestReconstruct_SettledRowsReplayAtEveryPosition(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		state   job.State
		outcome job.Outcome
	}{
		{job.Fetching, job.OutcomeFailed},
		{job.Assessing, job.OutcomeFailed},
		{job.Repairing, job.OutcomeFailed},
		{job.Extracting, job.OutcomeFailed},
		{job.Finalizing, job.OutcomeFailed},
		{job.Finalizing, job.OutcomeOK},
	} {
		t.Run(tc.state.String()+"_"+tc.outcome.String(), func(t *testing.T) {
			v := job.StateView{State: tc.state, Outcome: tc.outcome, Assessed: tc.state != job.Fetching}
			j, err := reconstruct("id", "n", job.Policy{}, v, job.IntentRun, now)
			if err != nil {
				t.Fatalf("reconstruct: %v", err)
			}
			got := j.Snapshot().State
			if got.State != tc.state {
				t.Errorf("State = %v, want %v", got.State, tc.state)
			}
			if got.Outcome != tc.outcome {
				t.Errorf("Outcome = %v, want %v", got.Outcome, tc.outcome)
			}
			if got.Next != job.StateUnset {
				t.Errorf("Next = %v, want StateUnset — Finish clears it when it settles", got.Next)
			}
			if got.Activity != job.ActNone {
				t.Errorf("Activity = %v, want ActNone — Finish clears it when it settles", got.Activity)
			}
		})
	}
}

// TestRestore_PreservesPolicy pins that a restored job comes back able to do
// what it was permitted to do. reconstruct built every job with job.Policy{}
// until this change, and a zero Policy denies all four permissions — so a
// restored job would silently neither verify, repair, unpack nor delete.
func TestRestore_PreservesPolicy(t *testing.T) {
	want := job.Policy{Verify: true, Repair: true, Unpack: true, Delete: true}
	st := &fakeStore{}
	st.seed([]Persisted{{
		ID:     "j1",
		Header: Header{Name: "j1"},
		Policy: want,
		State:  job.StateView{State: job.Fetching},
	}})
	d := newTestDispatcher(t, withStore(st))

	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	j, ok := d.lookup("j1")
	if !ok {
		t.Fatal("job was not registered")
	}
	if got := j.Policy(); got != want {
		t.Fatalf("restored Policy = %+v, want %+v", got, want)
	}
}

// TestRestore_PreservesSortKeyAndResumesAbove pins both halves of queue-order
// restoration with one set of rows.
//
// The seeded keys are non-contiguous and non-zero on purpose. Contiguous keys
// starting at 0 are exactly what a restore that reassigns from scratch would
// invent, so they cannot tell a preserved key from a fresh one.
//
// The second assertion is on sortKeyOf rather than on List order, for the same
// reason: register appends unconditionally, so a job added after restore is
// last in d.order whether or not the sequence resumed.
func TestRestore_PreservesSortKeyAndResumesAbove(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{
		{ID: "a", SortKey: 10, Header: Header{Name: "a"}},
		{ID: "b", SortKey: 50, Header: Header{Name: "b"}},
		{ID: "c", SortKey: 100, Header: Header{Name: "c"}},
	})
	d := newTestDispatcher(t, withStore(st))

	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for id, want := range map[string]int64{"a": 10, "b": 50, "c": 100} {
		if got := d.sortKeyOf(id); got != want {
			t.Errorf("restored %s sortKey = %d, want %d — restore reassigned it", id, got, want)
		}
	}
	if err := d.Add(job.New("fresh", "fresh", job.Policy{}), Header{Name: "fresh"}); err != nil {
		t.Fatalf("Add after restore: %v", err)
	}
	if got := d.sortKeyOf("fresh"); got <= 100 {
		t.Fatalf("post-restore Add got sortKey %d, want > 100 — the sequence did not resume, so the next job sorts ahead of restored ones", got)
	}
}

// TestRestore_DoesNotRewriteRowsItJustRead pins the spurious-Save regression
// directly. A restore that registers with a fresh sequence leaves d.written
// holding the stored key while the live entry carries a new one, so the first
// persistIfChanged finds a difference that is not there and renumbers every
// restored row in the store.
//
// Nothing else would report it: the queue simply comes back in a different
// order after the SECOND restart.
func TestRestore_DoesNotRewriteRowsItJustRead(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{
		{ID: "a", SortKey: 10, Header: Header{Name: "a"}},
		{ID: "b", SortKey: 50, Header: Header{Name: "b"}},
	})
	d := newTestDispatcher(t, withStore(st))
	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	before := st.saveCount()

	for _, j := range d.snapshotOrder() {
		d.persistIfChanged(context.Background(), j)
	}

	if got := st.saveCount() - before; got != 0 {
		t.Fatalf("a quiet pass over restored jobs issued %d Save calls, want 0 — restore and persistIfChanged disagree about what was stored", got)
	}
}
