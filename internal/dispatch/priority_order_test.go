package dispatch

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
)

// walkIDs is the tick's walk order as job IDs.
func walkIDs(d *Dispatcher) []string {
	js := d.snapshotOrder()
	out := make([]string, 0, len(js))
	for _, j := range js {
		out = append(out, j.ID())
	}
	return out
}

func addAt(t *testing.T, d *Dispatcher, id string, p constants.Priority) *job.Job {
	t.Helper()
	j := job.New(id, id, job.Policy{})
	if err := d.Add(context.Background(), j, Header{Name: id, Priority: int(p)}); err != nil {
		t.Fatalf("Add(%s): %v", id, err)
	}
	return j
}

// TestTick_WalksInPriorityOrderWithPositionAsTheTieBreak pins the dispatch
// order: Force, then High, then Normal, then Low, and queue position within
// one priority. The jobs are added in an order that disagrees with both halves
// — the lowest priority first, and the two Normal jobs n1 then n2 — so neither
// insertion order nor a sort without the position tie-break produces want.
func TestTick_WalksInPriorityOrderWithPositionAsTheTieBreak(t *testing.T) {
	d := newTestDispatcher(t)
	addAt(t, d, "low", constants.LowPriority)
	addAt(t, d, "n1", constants.NormalPriority)
	addAt(t, d, "high", constants.HighPriority)
	addAt(t, d, "n2", constants.NormalPriority)
	addAt(t, d, "force", constants.ForcePriority)

	want := []string{"force", "high", "n1", "n2", "low"}
	if got := walkIDs(d); !slices.Equal(got, want) {
		t.Fatalf("tick walk order = %v, want %v", got, want)
	}
}

// TestTick_PositionTieBreakHoldsBeyondInsertionSortThreshold pins the
// position tie-break on a queue large enough to matter. slices.SortFunc
// insertion-sorts fewer than 12 elements, which keeps equal elements in place,
// so on a small fixture a comparator with no tie-break still reads as stable.
// Forty jobs, every seventh High, takes it past that.
func TestTick_PositionTieBreakHoldsBeyondInsertionSortThreshold(t *testing.T) {
	d := newTestDispatcher(t)
	var high, normal []string
	for i := range 40 {
		id := fmt.Sprintf("n%02d", i)
		p := constants.NormalPriority
		if i%7 == 3 {
			p = constants.HighPriority
		}
		addAt(t, d, id, p)
		if p == constants.HighPriority {
			high = append(high, id)
		} else {
			normal = append(normal, id)
		}
	}
	want := append(high, normal...)

	if got := walkIDs(d); !slices.Equal(got, want) {
		t.Fatalf("tick walk order = %v, want %v", got, want)
	}
	rows := d.List()
	listed := make([]string, 0, len(rows))
	for _, r := range rows {
		listed = append(listed, r.ID)
	}
	if !slices.Equal(listed, want) {
		t.Fatalf("List order = %v, want %v", listed, want)
	}
}

// TestList_FollowsDispatchOrder pins that List — what the API's queue listing,
// the UI and the downloader's article dispatch read — shows the order the tick
// walks, not insertion order. Same fixture as the walk-order test, so the two
// orders it rules out (insertion, and no tie-break) are both wrong here too.
func TestList_FollowsDispatchOrder(t *testing.T) {
	d := newTestDispatcher(t)
	addAt(t, d, "low", constants.LowPriority)
	addAt(t, d, "n1", constants.NormalPriority)
	addAt(t, d, "high", constants.HighPriority)
	addAt(t, d, "n2", constants.NormalPriority)
	addAt(t, d, "force", constants.ForcePriority)

	rows := d.List()
	listed := make([]string, 0, len(rows))
	for _, r := range rows {
		listed = append(listed, r.ID)
	}
	if want := walkIDs(d); !slices.Equal(listed, want) {
		t.Fatalf("List order = %v, want the tick's walk order %v", listed, want)
	}
	if want := []string{"force", "high", "n1", "n2", "low"}; !slices.Equal(listed, want) {
		t.Fatalf("List order = %v, want %v", listed, want)
	}
}

// TestTick_HigherPriorityWinsTheOnlyLease is the behaviour the walk order
// exists for: with one lease, the job added last at a higher priority gets it.
func TestTick_HigherPriorityWinsTheOnlyLease(t *testing.T) {
	d := newTestDispatcher(t, withCaps(1, 1))
	first := addAt(t, d, "first", constants.NormalPriority)
	urgent := addAt(t, d, "urgent", constants.HighPriority)

	d.tick(context.Background())
	d.tick(context.Background())

	if !d.q.Render(urgent).Running {
		t.Error("urgent is not running — the higher-priority job must win the only lease")
	}
	if d.q.Render(first).Running {
		t.Error("first is running — a Normal job must wait behind a High one")
	}
}

// TestSetPriority_TakesEffectOnTheNextTick pins that SetPriority needs nothing
// else from its caller: the order is computed from the header at each walk.
func TestSetPriority_TakesEffectOnTheNextTick(t *testing.T) {
	d := newTestDispatcher(t, withCaps(1, 1))
	first := addAt(t, d, "first", constants.NormalPriority)
	second := addAt(t, d, "second", constants.NormalPriority)

	if err := d.SetPriority("second", int(constants.ForcePriority)); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())

	if !d.q.Render(second).Running {
		t.Error("second is not running — a priority raised before the tick must win the only lease")
	}
	if d.q.Render(first).Running {
		t.Error("first is running — it was outranked before the tick")
	}
}

// TestRestore_KeepsPriorityOrder pins the order across a restart. Priority is
// a header field, and the store returns the rows in SortKey order, so the
// restored walk order must come from the header rather than from Load.
func TestRestore_KeepsPriorityOrder(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{
		{ID: "a", SortKey: 1, Header: Header{Name: "a", Priority: int(constants.NormalPriority)}},
		{ID: "b", SortKey: 2, Header: Header{Name: "b", Priority: int(constants.LowPriority)}},
		{ID: "c", SortKey: 3, Header: Header{Name: "c", Priority: int(constants.HighPriority)}},
		{ID: "d", SortKey: 4, Header: Header{Name: "d", Priority: int(constants.NormalPriority)}},
	})
	d := newTestDispatcher(t, withStore(st))

	if got, want := walkIDs(d), []string{"c", "a", "d", "b"}; !slices.Equal(got, want) {
		t.Fatalf("restored walk order = %v, want %v", got, want)
	}
}
