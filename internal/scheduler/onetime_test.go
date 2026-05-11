package scheduler

import (
	"testing"
	"time"
)

// L12: Verify OneshotQueue bounded growth — Due() removes fired events.

func TestOneshotQueue_AddAndDue(t *testing.T) {
	t.Parallel()
	q := NewOneshotQueue()

	now := time.Now()
	past := now.Add(-1 * time.Minute)
	future := now.Add(1 * time.Hour)

	q.Add(Oneshot{FireAt: past, Action: "resume", Arg: "server1"})
	q.Add(Oneshot{FireAt: future, Action: "resume", Arg: "server2"})

	if q.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", q.Len())
	}

	// Due at 'now' should return only the past event.
	due := q.Due(now)
	if len(due) != 1 {
		t.Fatalf("Due(now) returned %d events, want 1", len(due))
	}
	if due[0].Arg != "server1" {
		t.Errorf("Due event Arg = %q, want %q", due[0].Arg, "server1")
	}

	// Queue should now have only 1 remaining.
	if q.Len() != 1 {
		t.Fatalf("Len() after Due = %d, want 1", q.Len())
	}
}

func TestOneshotQueue_BoundedGrowth(t *testing.T) {
	t.Parallel()
	q := NewOneshotQueue()

	now := time.Now()

	// Add 1000 events that fire immediately.
	for i := range 1000 {
		q.Add(Oneshot{
			FireAt: now.Add(-time.Duration(i) * time.Second),
			Action: "resume",
		})
	}
	if q.Len() != 1000 {
		t.Fatalf("Len() = %d, want 1000", q.Len())
	}

	// Drain all due events.
	due := q.Due(now)
	if len(due) != 1000 {
		t.Fatalf("Due(now) returned %d, want 1000", len(due))
	}

	// Queue should be empty — no unbounded growth.
	if q.Len() != 0 {
		t.Fatalf("Len() after drain = %d, want 0", q.Len())
	}
}

func TestOneshotQueue_DueAtExactFireTime(t *testing.T) {
	t.Parallel()
	q := NewOneshotQueue()

	now := time.Now()
	q.Add(Oneshot{FireAt: now, Action: "unpause"})

	// Due(now) should include events where FireAt == now (not strictly after).
	due := q.Due(now)
	if len(due) != 1 {
		t.Fatalf("Due(exactly at FireAt) returned %d, want 1", len(due))
	}
}

func TestOneshotQueue_EmptyDue(t *testing.T) {
	t.Parallel()
	q := NewOneshotQueue()

	now := time.Now()
	q.Add(Oneshot{FireAt: now.Add(1 * time.Hour), Action: "future"})

	due := q.Due(now)
	if len(due) != 0 {
		t.Fatalf("Due(now) should return empty for future events, got %d", len(due))
	}
	if q.Len() != 1 {
		t.Fatalf("Len() should remain 1, got %d", q.Len())
	}
}

func TestOneshotQueue_ConcurrentAddDue(t *testing.T) {
	t.Parallel()
	q := NewOneshotQueue()
	now := time.Now()

	// Concurrent adds and drains — verify no panic under -race.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 100 {
			q.Add(Oneshot{FireAt: now.Add(-time.Duration(i) * time.Millisecond), Action: "a"})
		}
	}()

	// Drain concurrently.
	var total int
	for range 50 {
		total += len(q.Due(now))
	}
	<-done
	// Final drain.
	total += len(q.Due(now))

	// All 100 events should have been retrieved exactly once.
	remaining := q.Len()
	if total+remaining != 100 {
		t.Errorf("total drained (%d) + remaining (%d) = %d, want 100", total, remaining, total+remaining)
	}
}
