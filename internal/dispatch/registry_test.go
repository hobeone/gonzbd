package dispatch

import (
	"slices"
	"testing"

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
	h := Header{Name: "movie", Category: "tv", Priority: 2, Bytes: 4096}
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
	if !d.claimLaunched("j1") {
		t.Error("claimLaunched(j1) = false after remove — a reused ID would be permanently unlaunchable")
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
