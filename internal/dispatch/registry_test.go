package dispatch

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
)

func TestAdd_RejectsADuplicateID(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{Name: "n"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := d.Add(job.New("j1", "other", job.Policy{}), Header{Name: "other"}); err == nil {
		t.Fatal("second Add with the same ID returned nil, want an error — the registry is keyed by ID and a silent overwrite would strand the first job's resources")
	}
}

func TestList_PreservesInsertionOrder(t *testing.T) {
	d := newTestDispatcher(t)
	for _, id := range []string{"c", "a", "b"} {
		if err := d.Add(job.New(id, id, job.Policy{}), Header{Name: id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}
	got := d.List()
	want := []string{"c", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("List returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("row %d is %s, want %s — queue order is the priority policy and List must not reorder it", i, got[i].ID, want[i])
		}
	}
}

func TestList_CarriesTheHeaderAndTheView(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	h := Header{Name: "movie", Category: "tv", Priority: 2, Bytes: 4096, Added: 1700000000}
	if err := d.Add(j, h); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := d.List()
	if len(got) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(got))
	}
	if got[0].Header != h {
		t.Errorf("Header = %+v, want %+v", got[0].Header, h)
	}
	if got[0].View != d.q.Render(j) {
		t.Errorf("View = %+v, want %+v", got[0].View, d.q.Render(j))
	}
}

// TestRemove_PreservesOrderOfRemainingEntries pins that remove is not a
// swap-with-last deletion: queue order is the priority policy, and swapping
// the last entry into a removed slot would silently reorder the jobs behind
// it.
func TestRemove_PreservesOrderOfRemainingEntries(t *testing.T) {
	d := newTestDispatcher(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := d.Add(job.New(id, id, job.Policy{}), Header{Name: id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}

	d.remove("b")

	got := d.List()
	want := []string{"a", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("List returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("row %d is %s, want %s — remove must preserve the order of the remaining entries", i, got[i].ID, want[i])
		}
	}
}

// TestRemove_PrunesTheWrittenRecord pins that remove also deletes the job's
// d.written entry, not just its registry entries: without this, d.written
// grows without bound as jobs are evicted, and a reused job ID would have its
// first Save wrongly suppressed by comparison against the dead job's stale
// row.
func TestRemove_PrunesTheWrittenRecord(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Add(job.New("j1", "n", job.Policy{}), Header{Name: "n"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.markWritten(Persisted{ID: "j1", Header: Header{Name: "n"}, State: job.StateView{State: job.Fetching}})
	if _, ok := d.lastWritten("j1"); !ok {
		t.Fatal("setup: markWritten did not record j1")
	}

	d.remove("j1")

	if _, ok := d.lastWritten("j1"); ok {
		t.Error("lastWritten(j1) = ok after remove, want false — remove must prune d.written alongside d.byID and d.order")
	}
}

// TestRemove_PrunesTheResidentAndLaunchedFlags is the same claim as the test
// above for the two maps it did not cover. Both were leaking, and both are
// worse than a leak: a stale resident entry makes reconcileResidency skip
// hydration for a reused job ID (its hydrate arm requires !d.isResident(id)),
// and a stale launched entry makes claimLaunched return false forever, so the
// job is permanently unlaunchable.
//
// It asserts through the same accessors production uses rather than reading
// the maps directly, so it pins the observable consequence rather than the
// storage.
func TestRemove_PrunesTheResidentAndLaunchedFlags(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Add(job.New("j1", "n", job.Policy{}), Header{Name: "n"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.markResident("j1")
	if !d.claimLaunched("j1") {
		t.Fatal("setup: claimLaunched(j1) = false on a fresh dispatcher")
	}

	d.remove("j1")

	if d.isResident("j1") {
		t.Error("isResident(j1) = true after remove — a reused ID would never hydrate, since reconcileResidency only hydrates when !isResident")
	}
	d.mu.Lock()
	_, launched := d.launched["j1"]
	d.mu.Unlock()
	if launched {
		t.Error("launched[j1] = true after remove")
	}

	// Re-add to simulate ID reuse: must be launchable.
	if err := d.Add(job.New("j1", "n", job.Policy{}), Header{Name: "n"}); err != nil {
		t.Fatalf("re-add j1: %v", err)
	}
	if !d.claimLaunched("j1") {
		t.Error("claimLaunched(j1) = false after re-register — a reused ID would be permanently unlaunchable")
	}
}

func TestList_EmptyRegistryReturnsEmptyNonNil(t *testing.T) {
	d := newTestDispatcher(t)
	got := d.List()
	if got == nil {
		t.Fatal("List() = nil on an empty registry, want an empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("List() has %d rows, want 0", len(got))
	}
}

// TestSortKey_ReproducesQueueOrderAcrossRemoval pins the enumeration that lets
// queue order be persisted as a plain insertion sequence rather than a
// position needing a renumbering pass.
//
// Only two operations change d.order, and neither invalidates such a key: Add
// appends, and remove deletes in place with slices.Delete, which preserves the
// relative order of what survives. `git grep -n 'd\.order = ' internal/dispatch`
// returns 2 lines — register's append and remove's slices.Delete — so those
// two operations are the whole population.
//
// The keys are asserted EXACTLY rather than with slices.IsSorted. IsSorted is
// non-strict, so an implementation that never assigns a sequence at all yields
// [0 0 0] and passes while the feature is entirely broken.
func TestSortKey_ReproducesQueueOrderAcrossRemoval(t *testing.T) {
	d := newTestDispatcher(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := d.Add(job.New(id, id, job.Policy{}), Header{Name: id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}
	d.remove("b")

	rows := d.List()
	got := make([]string, 0, len(rows))
	keys := make([]int64, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ID)
		keys = append(keys, d.sortKeyOf(r.ID))
	}
	if want := []string{"a", "c", "d"}; !slices.Equal(got, want) {
		t.Fatalf("order after removal = %v, want %v", got, want)
	}
	// The gap at 1 is the point: b's key is retired with b, and c and d keep
	// the keys they were registered with. A position would have renumbered
	// them to 1 and 2.
	if want := []int64{0, 2, 3}; !slices.Equal(keys, want) {
		t.Fatalf("sort keys = %v, want %v", keys, want)
	}
}

// TestRegister_TakesTheSequenceItIsGivenAndAdvancesPast pins register's own
// contract, rather than reaching it only through Add and restore.
//
// The two callers want opposite things from it — Add wants the next unused
// sequence, restore wants the one already on disk — and what makes both safe is
// that register advances d.nextSeq past whatever it was handed. Testing it
// directly is what keeps that property from being an accident of the two
// callers' current shapes.
func TestRegister_TakesTheSequenceItIsGivenAndAdvancesPast(t *testing.T) {
	d := newTestDispatcher(t)

	if err := d.register(job.New("a", "a", job.Policy{}), Header{Name: "a"}, 42); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := d.sortKeyOf("a"); got != 42 {
		t.Fatalf("sortKeyOf(a) = %d, want 42 — register did not take the sequence it was given", got)
	}

	// A LOWER sequence must not drag the counter backwards, or a later Add
	// would collide with a key already in use.
	if err := d.register(job.New("b", "b", job.Policy{}), Header{Name: "b"}, 7); err != nil {
		t.Fatalf("register(7): %v", err)
	}
	if err := d.Add(job.New("c", "c", job.Policy{}), Header{Name: "c"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := d.sortKeyOf("c"); got != 43 {
		t.Fatalf("Add after register(42) then register(7) got key %d, want 43 — the counter moved backwards", got)
	}

	if err := d.register(job.New("a", "dup", job.Policy{}), Header{Name: "dup"}, 99); err == nil {
		t.Error("register accepted a duplicate ID; a silent overwrite would strand the first job's resources")
	}
}

// TestAdd_ConcurrentCallsGetDistinctSortKeys pins that the sequence is
// allocated under the same lock span that uses it.
//
// An Add that reads d.nextSeq, releases d.mu, and only then registers hands two
// concurrent callers the SAME key. Nothing fails at the time: both jobs
// register, both persist, and the queue looks right. The damage appears on the
// NEXT restart, when the ID tiebreak orders the colliding pair alphabetically
// instead of by insertion.
func TestAdd_ConcurrentCallsGetDistinctSortKeys(t *testing.T) {
	d := newTestDispatcher(t)
	const n = 64

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			id := fmt.Sprintf("j%02d", i)
			if err := d.Add(job.New(id, id, job.Policy{}), Header{Name: id}); err != nil {
				t.Errorf("Add(%s): %v", id, err)
			}
		})
	}
	wg.Wait()

	seen := make(map[int64]string, n)
	for _, r := range d.List() {
		k := d.sortKeyOf(r.ID)
		if other, dup := seen[k]; dup {
			t.Fatalf("%s and %s both got sort key %d — two Adds allocated the same sequence", other, r.ID, k)
		}
		seen[k] = r.ID
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct sort keys across %d jobs, want %d", len(seen), n, n)
	}
}

// TestAdd_RefusedOnAStoppedDispatcher pins that a job offered to a dead
// dispatcher is rejected rather than accepted and stranded.
//
// Without the check Add succeeded: it allocated a sequence, registered the job,
// and primed d.wake with no loop left to read it. The caller got nil and the
// job was never ticked, never persisted and never run — the worst shape of
// failure, since nothing anywhere reports it.
func TestAdd_RefusedOnAStoppedDispatcher(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := d.Add(job.New("j1", "n", job.Policy{}), Header{Name: "n"}); err == nil {
		t.Fatal("Add on a stopped dispatcher returned nil; the job would be registered with no loop to run it")
	}
	if got := len(d.List()); got != 0 {
		t.Errorf("registry holds %d jobs after a refused Add, want 0", got)
	}
}

// TestAdd_RefusedWhileRestoring pins that a job cannot be interleaved into the
// registry while restore is rebuilding it.
//
// Two things go wrong if it can. The intruder takes a sequence from the
// counter before restore has raised it past the stored keys, so it collides
// with a row still to be registered; and restore's rollback removes only the
// IDs it registered itself, so a failing restore leaves the intruder behind in
// a dispatcher that never started.
func TestAdd_RefusedWhileRestoring(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{{ID: "stored", SortKey: 10, Header: Header{Name: "stored"}}})
	d := newTestDispatcher(t, withStore(st))

	var addErr error
	st.loadHook = func() {
		addErr = d.Add(job.New("intruder", "n", job.Policy{}), Header{Name: "n"})
	}
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	if addErr == nil {
		t.Fatal("Add during restore returned nil; it would take a sequence before restore raised the counter past the stored keys")
	}
	for _, r := range d.List() {
		if r.ID == "intruder" {
			t.Error("the intruding job is in the registry; a failing restore's rollback would not have removed it")
		}
	}
}

// TestDispatcherRow_ReturnsOneJobWithoutRenderingTheRest is #436: a header-tier
// caller must not pay manifest-tier cost. It asserts the cheap path exists and
// agrees with List, so the two cannot drift.
func TestDispatcherRow_ReturnsOneJobWithoutRenderingTheRest(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Add(job.New("a", "Job A", job.Policy{}), Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}
	if err := d.Add(job.New("b", "Job B", job.Policy{}), Header{Name: "Job B"}); err != nil {
		t.Fatalf("Add(b): %v", err)
	}

	row, ok := d.Row("b")
	if !ok {
		t.Fatal("Row(b) must find the job")
	}
	if row.ID != "b" || row.Header.Name != "Job B" {
		t.Fatalf("Row(b) = %+v, want ID b / Name Job B", row)
	}

	var want Row
	for _, r := range d.List() {
		if r.ID == "b" {
			want = r
		}
	}
	if row.View != want.View {
		t.Fatalf("Row and List disagree for b:\n Row = %+v\nList = %+v", row.View, want.View)
	}

	if _, ok := d.Row("nope"); ok {
		t.Fatal("Row of an unknown id must report not-found")
	}
}

func TestDispatcher_SetWarning(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(j, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := d.SetWarning("unknown", "warn"); err == nil {
		t.Fatal("SetWarning on unknown job should error")
	}

	if err := d.SetWarning("j1", "disk low"); err != nil {
		t.Fatalf("SetWarning: %v", err)
	}

	row, ok := d.Row("j1")
	if !ok {
		t.Fatal("Row(j1) not found")
	}
	if row.Header.Warning != "disk low" {
		t.Fatalf("Header.Warning = %q, want 'disk low'", row.Header.Warning)
	}
}

func TestDispatcher_Mutators(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(j, Header{Name: "Job 1", Filename: "file1.nzb"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Unknown job errors
	for _, fn := range []func() error{
		func() error { return d.SetPriority("unknown", 1) },
		func() error { return d.SetName("unknown", "n") },
		func() error { return d.SetCategory("unknown", "c") },
		func() error { return d.SetPP("unknown", 2) },
		func() error { return d.SetScript("unknown", "s") },
	} {
		if err := fn(); err == nil {
			t.Fatal("expected error on unknown job")
		}
	}

	// Priority
	if err := d.SetPriority("j1", 2); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	// Name
	if err := d.SetName("j1", "New Name"); err != nil {
		t.Fatalf("SetName: %v", err)
	}
	// Category
	if err := d.SetCategory("j1", "tv"); err != nil {
		t.Fatalf("SetCategory: %v", err)
	}
	// PP
	if err := d.SetPP("j1", 3); err != nil {
		t.Fatalf("SetPP: %v", err)
	}
	// Script
	if err := d.SetScript("j1", "notify.sh"); err != nil {
		t.Fatalf("SetScript: %v", err)
	}

	row, ok := d.Row("j1")
	if !ok {
		t.Fatal("Row(j1) not found")
	}
	if row.Header.Priority != 2 {
		t.Errorf("Priority = %d, want 2", row.Header.Priority)
	}
	if row.Header.Name != "New Name" || j.Name() != "New Name" {
		t.Errorf("Name = %q / %q, want 'New Name'", row.Header.Name, j.Name())
	}
	if row.Header.Category != "tv" {
		t.Errorf("Category = %q, want 'tv'", row.Header.Category)
	}
	if row.Header.PP != 3 || !j.Policy().Delete {
		t.Errorf("PP = %d, Policy = %+v, want PP 3 / Delete: true", row.Header.PP, j.Policy())
	}
	if row.Header.Script != "notify.sh" {
		t.Errorf("Script = %q, want 'notify.sh'", row.Header.Script)
	}

	if got := d.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
}

func TestSetPriority_Validation(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test-job", job.Policy{})
	if err := d.Add(j, Header{Name: "test-job"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := d.SetPriority("j1", 99); err == nil {
		t.Fatalf("SetPriority(j1, 99) = nil, want error for invalid priority")
	}

	if err := d.SetPriority("j1", int(constants.HighPriority)); err != nil {
		t.Fatalf("SetPriority(j1, HighPriority): %v", err)
	}
	row, ok := d.Row("j1")
	if !ok {
		t.Fatalf("Row(j1) not found")
	}
	if row.Header.Priority != int(constants.HighPriority) {
		t.Errorf("Priority = %d, want %d", row.Header.Priority, constants.HighPriority)
	}
}

func TestAdd_NormalizesZeroOrNegativeAdded(t *testing.T) {
	d := newTestDispatcher(t)
	before := time.Now().UTC().Unix()

	// Zero Added
	j1 := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(j1, Header{Name: "Job 1", Added: 0}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	if j1.Added().Unix() < before {
		t.Errorf("j1.Added() = %v, want >= %v (zero Added must be normalized to current time)", j1.Added().Unix(), before)
	}

	// Negative Added
	j2 := job.New("j2", "Job 2", job.Policy{})
	if err := d.Add(j2, Header{Name: "Job 2", Added: -100}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}
	if j2.Added().Unix() < before {
		t.Errorf("j2.Added() = %v, want >= %v (negative Added must be normalized to current time)", j2.Added().Unix(), before)
	}
}
