package downloader

import (
	"testing"
)

func TestMapTracker_Coverage(t *testing.T) {
	mt := newMapTracker()

	// Test IncrementInFlightLocked and DecrementInFlightLocked with values > 1
	mt.IncrementInFlightLocked("key1")
	mt.IncrementInFlightLocked("key1")
	if mt.InFlightLocked("key1") != 2 {
		t.Errorf("expected InFlightLocked to be 2, got %d", mt.InFlightLocked("key1"))
	}

	mt.DecrementInFlightLocked("key1")
	if mt.InFlightLocked("key1") != 1 {
		t.Errorf("expected InFlightLocked to be 1, got %d", mt.InFlightLocked("key1"))
	}

	mt.DecrementInFlightLocked("key1")
	if mt.InFlightLocked("key1") != 0 {
		t.Errorf("expected InFlightLocked to be 0, got %d", mt.InFlightLocked("key1"))
	}

	// Test UnmarkTriedLocked on non-existent key
	mt.UnmarkTriedLocked("non-existent", 0)

	// Test UnmarkTriedLocked where mask is not empty after unmarking
	mask := serverMask{}
	mask.set(0)
	mask.set(1)
	mt.SetTriedLocked("key2", mask)

	mt.UnmarkTriedLocked("key2", 0)
	gotMask, ok := mt.TryListLocked("key2")
	if !ok {
		t.Fatal("expected key2 to exist in tryList")
	}
	if gotMask.has(0) {
		t.Error("expected server 0 to be unmarked")
	}
	if !gotMask.has(1) {
		t.Error("expected server 1 to remain marked")
	}

	// Test UnmarkTriedLocked where mask becomes empty
	mt.UnmarkTriedLocked("key2", 1)
	_, ok = mt.TryListLocked("key2")
	if ok {
		t.Error("expected key2 to be deleted from tryList when mask is empty")
	}
}
