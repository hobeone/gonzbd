package downloader

import (
	"sync"
	"testing"
)

// k1 and k2 are two distinct tracker keys. dispatchTracker is agnostic about
// what a key denotes — it stores masks and counters against whatever identity
// it is handed — so these stand in for any two articles. That they differ in
// artIdx rather than jobID is arbitrary here; the cross-job case has its own
// test below, because that separation is the reason the key is a pair.
var (
	k1 = articleKey{jobID: "j1", artIdx: 1}
	k2 = articleKey{jobID: "j1", artIdx: 2}
)

func TestTracker_InFlight(t *testing.T) {
	tr := newDispatchTracker()

	t.Run("increment and decrement", func(t *testing.T) {
		tr.Lock()
		tr.IncrementInFlightLocked(k1)
		tr.IncrementInFlightLocked(k1)
		if got := tr.InFlightLocked(k1); got != 2 {
			t.Errorf("expected InFlightLocked to be 2, got %d", got)
		}
		tr.Unlock()

		tr.DecrementInFlight(k1)
		tr.Lock()
		if got := tr.InFlightLocked(k1); got != 1 {
			t.Errorf("expected InFlightLocked to be 1, got %d", got)
		}
		tr.Unlock()

		tr.DecrementInFlight(k1)
		tr.Lock()
		if got := tr.InFlightLocked(k1); got != 0 {
			t.Errorf("expected InFlightLocked to be 0, got %d", got)
		}
		tr.Unlock()
	})

	t.Run("isolation", func(t *testing.T) {
		tr.Lock()
		tr.IncrementInFlightLocked(k1)
		tr.IncrementInFlightLocked(k2)
		if tr.InFlightLocked(k1) != 1 || tr.InFlightLocked(k2) != 1 {
			t.Error("expected isolated in-flight counters")
		}
		tr.Unlock()
	})
}

func TestTracker_TryList(t *testing.T) {
	tr := newDispatchTracker()

	t.Run("set and clear", func(t *testing.T) {
		mask := serverMask{}
		mask.set(1)

		tr.Lock()
		tr.SetTriedLocked(k1, mask)
		got, ok := tr.TryListLocked(k1)
		if !ok || !got.has(1) {
			t.Error("expected tried mask to be set and retrieved")
		}
		tr.Unlock()

		tr.ClearTried(k1)
		tr.Lock()
		_, ok = tr.TryListLocked(k1)
		if ok {
			t.Error("expected tried mask to be cleared")
		}
		tr.Unlock()
	})

	t.Run("unmark tried", func(t *testing.T) {
		mask := serverMask{}
		mask.set(1)
		mask.set(2)

		tr.Lock()
		tr.SetTriedLocked(k1, mask)
		tr.Unlock()

		// Unmark server 1.
		tr.UnmarkTried(k1, 1)

		tr.Lock()
		got, ok := tr.TryListLocked(k1)
		if !ok {
			t.Fatal("expected tried mask to exist")
		}
		if got.has(1) || !got.has(2) {
			t.Error("expected server 1 to be unmarked and server 2 to remain")
		}
		tr.Unlock()

		// Unmark server 2. This should clean up the key.
		tr.UnmarkTried(k1, 2)

		tr.Lock()
		_, ok = tr.TryListLocked(k1)
		if ok {
			t.Error("expected key to be removed when tried mask becomes empty")
		}
		tr.Unlock()
	})

	t.Run("unmark non-existent", func(t *testing.T) {
		tr.UnmarkTried(articleKey{jobID: "j1", artIdx: 404}, 1) // should not panic or error
	})
}

func TestTracker_Len(t *testing.T) {
	tr := newDispatchTracker()

	tryLen, inFlightLen := tr.Len()
	if tryLen != 0 || inFlightLen != 0 {
		t.Errorf("expected 0, 0, got %d, %d", tryLen, inFlightLen)
	}

	tr.Lock()
	tr.IncrementInFlightLocked(k1)
	mask := serverMask{}
	mask.set(0)
	tr.SetTriedLocked(k2, mask)
	tr.Unlock()

	tryLen, inFlightLen = tr.Len()
	if tryLen != 1 || inFlightLen != 1 {
		t.Errorf("expected 1, 1, got %d, %d", tryLen, inFlightLen)
	}
}

func TestTracker_Concurrency(t *testing.T) {
	tr := newDispatchTracker()
	const workers = 10
	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(workers * 2)

	// In-flight churners
	for i := range workers {
		go func(workerID int) {
			defer wg.Done()
			key := articleKey{jobID: "j1", artIdx: 10}
			for range iterations {
				tr.Lock()
				tr.IncrementInFlightLocked(key)
				tr.Unlock()

				tr.DecrementInFlight(key)
			}
		}(i)
	}

	// TryList churners
	for i := range workers {
		go func(workerID int) {
			defer wg.Done()
			key := articleKey{jobID: "j1", artIdx: 20}
			for range iterations {
				mask := serverMask{}
				mask.set(workerID)

				tr.Lock()
				tr.SetTriedLocked(key, mask)
				tr.Unlock()

				tr.UnmarkTried(key, workerID)
			}
		}(i)
	}

	wg.Wait()
}
