package queue

import "testing"

// The 64-bit boundary is where a word-packed bitset goes wrong: an
// off-by-one in the word index or shift silently reads a neighbouring
// article's flag. Sizes either side of a word boundary are therefore the
// cases worth pinning, not a round number like 100.
func TestBitsetSetGetAcrossWordBoundary(t *testing.T) {
	const n = 130
	b := newBitset(n)

	if b.Len() != n {
		t.Fatalf("Len() = %d, want %d", b.Len(), n)
	}
	for i := range n {
		if b.Get(i) {
			t.Fatalf("bit %d set in a fresh bitset", i)
		}
	}

	for _, i := range []int{0, 63, 64, 65, 127, 128, 129} {
		b.Set(i)
	}
	for _, i := range []int{0, 63, 64, 65, 127, 128, 129} {
		if !b.Get(i) {
			t.Errorf("Get(%d) = false after Set(%d)", i, i)
		}
	}
	// Every neighbour of a set bit must stay clear, which is what catches a
	// shift or word-index off-by-one.
	for _, i := range []int{1, 62, 66, 126} {
		if b.Get(i) {
			t.Errorf("Get(%d) = true; a neighbouring Set leaked into it", i)
		}
	}

	b.Clear(64)
	if b.Get(64) {
		t.Error("Get(64) = true after Clear(64)")
	}
	if !b.Get(65) {
		t.Error("Clear(64) also cleared bit 65")
	}
}

// Clone must not alias: JobProgress.clone deep-copies progress for every
// snapshot, and a shared backing array would let a snapshot mutate the live
// job.
func TestBitsetCloneIsIndependent(t *testing.T) {
	b := newBitset(70)
	b.Set(3)

	c := b.Clone()
	c.Set(69)
	c.Clear(3)

	if !b.Get(3) {
		t.Error("clearing the clone cleared the original")
	}
	if b.Get(69) {
		t.Error("setting the clone set the original")
	}
}

// The on-disk JSON shape stays []bool, so the conversion must round-trip
// exactly, including trailing padding bits that do not correspond to an
// article.
func TestBitsetBoolRoundTrip(t *testing.T) {
	in := make([]bool, 70)
	in[0], in[64], in[69] = true, true, true

	got := bitsetFromBools(in).ToBools()

	if len(got) != len(in) {
		t.Fatalf("round trip changed length: got %d, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("bit %d: got %v, want %v", i, got[i], in[i])
		}
	}
}

// TestBitsetOutOfRangeIsSafe pins the bounds-check branch on Get/Set/Clear:
// callers that pass a stale or corrupt index must get a safe no-op/false
// rather than a panic or a write past the end of words.
func TestBitsetOutOfRangeIsSafe(t *testing.T) {
	b := newBitset(10)

	for _, i := range []int{-1, 10, 100} {
		if b.Get(i) {
			t.Errorf("Get(%d) = true on a %d-bit set", i, b.Len())
		}
		b.Set(i)
		if b.Get(i) {
			t.Errorf("Set(%d) took effect on a %d-bit set", i, b.Len())
		}
		b.Clear(i)
	}
}

// TestNewBitsetNegativeSizeClampsToZero pins newBitset's defensive clamp:
// a negative size (which should never occur, but a caller could pass a
// miscomputed one) must not allocate a negative-length words slice.
func TestNewBitsetNegativeSizeClampsToZero(t *testing.T) {
	b := newBitset(-5)
	if b.Len() != 0 {
		t.Errorf("Len() = %d, want 0 for negative size", b.Len())
	}
}
