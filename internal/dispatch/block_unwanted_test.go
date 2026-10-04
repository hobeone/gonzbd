package dispatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestBlockUnwanted_PauseMovesNoneToBlockedAndPersists pins the move itself:
// a job at StateNone becomes Blocked, a pause is applied with it, the next
// tick persists the state, and the user's resume approves it.
func TestBlockUnwanted_PauseMovesNoneToBlockedAndPersists(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	moved, now, err := d.BlockUnwanted("a", true)
	if err != nil || !moved || now != unwanted.StateBlocked {
		t.Fatalf("BlockUnwanted = (%v, %d, %v), want (true, blocked, nil)", moved, now, err)
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause", in)
	}
	if got, ok := d.UnwantedState("a"); !ok || got != unwanted.StateBlocked {
		t.Errorf("UnwantedState = (%d, %v), want blocked", got, ok)
	}
	d.tick(context.Background())
	if p, ok := st.row("a"); !ok || p.Header.Unwanted != unwanted.StateBlocked {
		t.Errorf("persisted Unwanted = %d (row %v), want blocked", p.Header.Unwanted, ok)
	}
	if err := d.ResumeJob("a"); !errors.Is(err, ErrUnwantedBlocked) {
		t.Errorf("ResumeJob on the blocked job = %v, want ErrUnwantedBlocked", err)
	}
	if err := d.ResumeJobByUser("a"); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	if got, _ := d.UnwantedState("a"); got != unwanted.StateApproved {
		t.Errorf("after the user's resume UnwantedState = %d, want approved", got)
	}
}

// TestBlockUnwanted_WithoutPauseLeavesTheIntent pins that the fail action's
// call blocks but does not pause: its caller files the job.
func TestBlockUnwanted_WithoutPauseLeavesTheIntent(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	moved, _, err := d.BlockUnwanted("a", false)
	if err != nil || !moved {
		t.Fatalf("BlockUnwanted = (%v, %v), want moved", moved, err)
	}
	if in := j.Intent(); in != job.IntentRun {
		t.Errorf("Intent = %v, want IntentRun", in)
	}
}

// TestBlockUnwanted_LeavesBlockedAndApprovedAlone pins that only StateNone
// moves: an approved job is never blocked again, and a blocked one is not
// paused a second time.
func TestBlockUnwanted_LeavesBlockedAndApprovedAlone(t *testing.T) {
	for _, from := range []unwanted.State{unwanted.StateBlocked, unwanted.StateApproved} {
		d := newTestDispatcher(t)
		j := job.New("a", "Job A", job.PolicyFromPP(3))
		if err := d.Add(context.Background(), j, Header{Name: "Job A", Unwanted: from}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		moved, now, err := d.BlockUnwanted("a", true)
		if err != nil || moved || now != from {
			t.Errorf("from %d: BlockUnwanted = (%v, %d, %v), want (false, %d, nil)", from, moved, now, err, from)
		}
		if in := j.Intent(); in != job.IntentRun {
			t.Errorf("from %d: Intent = %v, want IntentRun: a refused block paused the job", from, in)
		}
	}
}

// TestBlockUnwanted_UnknownJob pins the not-found refusal.
func TestBlockUnwanted_UnknownJob(t *testing.T) {
	d := newTestDispatcher(t)
	if _, _, err := d.BlockUnwanted("nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("BlockUnwanted of an unknown id = %v, want ErrNotFound", err)
	}
	if _, ok := d.UnwantedState("nope"); ok {
		t.Error("UnwantedState of an unknown id reported found")
	}
}

// TestBlockUnwanted_ConcurrentCallsMoveOnce pins that the decision is taken
// under one lock: of many simultaneous calls exactly one makes the move.
func TestBlockUnwanted_ConcurrentCallsMoveOnce(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	const callers = 16
	var moves atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Go(func() {
			<-start
			moved, now, err := d.BlockUnwanted("a", true)
			if err != nil || now != unwanted.StateBlocked {
				t.Errorf("BlockUnwanted = (%v, %d, %v), want a blocked job", moved, now, err)
			}
			if moved {
				moves.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := moves.Load(); got != 1 {
		t.Errorf("%d calls reported making the move, want exactly 1", got)
	}
}
