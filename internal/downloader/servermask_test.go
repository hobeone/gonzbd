package downloader

import "testing"

// ---------------------------------------------------------------------------
// set + has round-trip — fast path (idx < 64)
// ---------------------------------------------------------------------------

func TestServerMask_SetHas_FastPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		idx  int
	}{
		{"bit 0", 0},
		{"bit 1", 1},
		{"bit 31", 31},
		{"bit 62", 62},
		{"bit 63", 63},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var m serverMask
			if m.has(tc.idx) {
				t.Fatalf("has(%d) = true on zero-value mask", tc.idx)
			}
			m.set(tc.idx)
			if !m.has(tc.idx) {
				t.Fatalf("has(%d) = false after set", tc.idx)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// set + has round-trip — slow path (idx >= 64)
// ---------------------------------------------------------------------------

func TestServerMask_SetHas_SlowPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		idx  int
	}{
		{"bit 64", 64},
		{"bit 65", 65},
		{"bit 127", 127},
		{"bit 128", 128},
		{"bit 200", 200},
		{"bit 1000", 1000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var m serverMask
			if m.has(tc.idx) {
				t.Fatalf("has(%d) = true on zero-value mask", tc.idx)
			}
			m.set(tc.idx)
			if !m.has(tc.idx) {
				t.Fatalf("has(%d) = false after set", tc.idx)
			}
			// Verify the fast path is not polluted.
			if m.fast[0] != 0 {
				t.Errorf("fast[0] = %d; want 0 (slow path should not touch fast)", m.fast[0])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// unset
// ---------------------------------------------------------------------------

func TestServerMask_Unset_FastPath(t *testing.T) {
	t.Parallel()
	var m serverMask

	m.set(5)
	m.set(10)
	m.unset(5)

	if m.has(5) {
		t.Error("has(5) = true after unset")
	}
	if !m.has(10) {
		t.Error("has(10) = false; unset(5) should not affect bit 10")
	}
}

func TestServerMask_Unset_SlowPath(t *testing.T) {
	t.Parallel()
	var m serverMask

	m.set(100)
	m.set(200)
	m.unset(100)

	if m.has(100) {
		t.Error("has(100) = true after unset")
	}
	if !m.has(200) {
		t.Error("has(200) = false; unset(100) should not affect bit 200")
	}
}

func TestServerMask_Unset_NoopOnUnallocated(t *testing.T) {
	t.Parallel()
	var m serverMask

	// Unset on a slow index that was never set should not panic.
	m.unset(999)
	if m.has(999) {
		t.Error("has(999) = true after unset on never-set bit")
	}
}

// ---------------------------------------------------------------------------
// isEmpty
// ---------------------------------------------------------------------------

func TestServerMask_IsEmpty_ZeroValue(t *testing.T) {
	t.Parallel()
	var m serverMask

	if !m.isEmpty() {
		t.Error("isEmpty() = false on zero-value mask")
	}
}

func TestServerMask_IsEmpty_AfterFastSet(t *testing.T) {
	t.Parallel()
	var m serverMask

	m.set(0)
	if m.isEmpty() {
		t.Error("isEmpty() = true after set(0)")
	}
	m.unset(0)
	if !m.isEmpty() {
		t.Error("isEmpty() = false after unsetting all bits")
	}
}

func TestServerMask_IsEmpty_AfterSlowSet(t *testing.T) {
	t.Parallel()
	var m serverMask

	m.set(128)
	if m.isEmpty() {
		t.Error("isEmpty() = true after set(128)")
	}
	m.unset(128)
	if !m.isEmpty() {
		t.Error("isEmpty() = false after unsetting all bits")
	}
}

func TestServerMask_IsEmpty_MixedFastSlow(t *testing.T) {
	t.Parallel()
	var m serverMask

	m.set(3)
	m.set(200)

	if m.isEmpty() {
		t.Error("isEmpty() = true with bits set in both fast and slow")
	}

	m.unset(3)
	if m.isEmpty() {
		t.Error("isEmpty() = true; bit 200 is still set")
	}

	m.unset(200)
	if !m.isEmpty() {
		t.Error("isEmpty() = false after unsetting all bits")
	}
}

// ---------------------------------------------------------------------------
// Multiple bits set simultaneously
// ---------------------------------------------------------------------------

func TestServerMask_MultipleBits(t *testing.T) {
	t.Parallel()
	var m serverMask

	indices := []int{0, 1, 31, 63, 64, 65, 127, 128, 255, 512}
	for _, idx := range indices {
		m.set(idx)
	}

	for _, idx := range indices {
		if !m.has(idx) {
			t.Errorf("has(%d) = false after setting multiple bits", idx)
		}
	}

	// Spot-check that unset indices are still false.
	for _, idx := range []int{2, 30, 62, 66, 129, 256, 513} {
		if m.has(idx) {
			t.Errorf("has(%d) = true; was never set", idx)
		}
	}
}

// ---------------------------------------------------------------------------
// Set is idempotent
// ---------------------------------------------------------------------------

func TestServerMask_SetIdempotent(t *testing.T) {
	t.Parallel()
	var m serverMask

	m.set(42)
	m.set(42)
	m.set(42)

	if !m.has(42) {
		t.Error("has(42) = false after triple set")
	}
	m.unset(42)
	if m.has(42) {
		t.Error("has(42) = true after unset")
	}
}

// ---------------------------------------------------------------------------
// Boundary: idx 63 (last fast) → idx 64 (first slow)
// ---------------------------------------------------------------------------

func TestServerMask_Boundary63_64(t *testing.T) {
	t.Parallel()
	var m serverMask

	m.set(63)
	m.set(64)

	if !m.has(63) {
		t.Error("has(63) = false")
	}
	if !m.has(64) {
		t.Error("has(64) = false")
	}

	// Unset one; the other must survive.
	m.unset(63)
	if m.has(63) {
		t.Error("has(63) = true after unset")
	}
	if !m.has(64) {
		t.Error("has(64) = false after unsetting 63")
	}
}

// ---------------------------------------------------------------------------
// has returns false for out-of-range slow indices that were never allocated
// ---------------------------------------------------------------------------

func TestServerMask_Has_UnallocatedSlow(t *testing.T) {
	t.Parallel()
	var m serverMask

	// The slow slice is nil — has must return false, not panic.
	if m.has(500) {
		t.Error("has(500) = true on zero-value mask")
	}

	// Allocate up to index 64 (slow slot 0), then query a higher slot.
	m.set(64)
	if m.has(200) {
		t.Error("has(200) = true on mask that only has bit 64 set")
	}
}

// ---------------------------------------------------------------------------
// Value-type copy semantics (map usage pattern)
// ---------------------------------------------------------------------------

func TestServerMask_CopyUpdateReplace(t *testing.T) {
	t.Parallel()

	tryList := make(map[string]serverMask)

	// First access: copy zero value, set, replace.
	mask := tryList["key"]
	mask.set(0)
	mask.set(64)
	tryList["key"] = mask

	// Retrieve and verify.
	got := tryList["key"]
	if !got.has(0) {
		t.Error("after replace, has(0) = false")
	}
	if !got.has(64) {
		t.Error("after replace, has(64) = false")
	}
}
