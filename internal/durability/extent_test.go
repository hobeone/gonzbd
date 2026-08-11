package durability

import "testing"

func TestBitmap_RoundTripsThroughBytes(t *testing.T) {
	b := NewBitmap(200)
	for _, i := range []int{0, 63, 64, 65, 199} {
		b.Set(i)
	}
	got, err := BitmapFromBytes(b.Bytes(), 200)
	if err != nil {
		t.Fatalf("BitmapFromBytes: %v", err)
	}
	if got.Count() != 5 {
		t.Fatalf("Count() = %d, want 5", got.Count())
	}
	for _, i := range []int{0, 63, 64, 65, 199} {
		if !got.Get(i) {
			t.Errorf("Get(%d) = false after round trip", i)
		}
	}
	if got.Get(1) || got.Get(198) {
		t.Error("round trip invented a set bit")
	}
}

// TestBitmapFromBytes_RejectsShortBuffer pins that a truncated persisted
// bitmap is an error rather than a silently short map. S3 requires that an
// article whose state cannot be established is Outstanding; a silently
// truncated bitmap would instead report those articles as not-durable
// without anyone noticing the record was damaged.
func TestBitmapFromBytes_RejectsShortBuffer(t *testing.T) {
	if _, err := BitmapFromBytes([]byte{0x01}, 200); err == nil {
		t.Fatal("BitmapFromBytes accepted a buffer too short for n=200")
	}
}

// TestNewBitmap_NegativeSizeClampsToZero pins that a negative n does not
// panic or underflow the word-count computation; it degenerates to an
// empty, zero-length bitmap instead.
func TestNewBitmap_NegativeSizeClampsToZero(t *testing.T) {
	b := NewBitmap(-5)
	if b.Len() != 0 {
		t.Fatalf("Len() = %d, want 0 for negative n", b.Len())
	}
	if b.Count() != 0 {
		t.Fatalf("Count() = %d, want 0 for negative n", b.Count())
	}
}

func TestBitmap_Len(t *testing.T) {
	b := NewBitmap(37)
	if b.Len() != 37 {
		t.Fatalf("Len() = %d, want 37", b.Len())
	}
}

func TestBitmap_OutOfRangeIsNoOp(t *testing.T) {
	b := NewBitmap(8)
	b.Set(-1)
	b.Set(8)
	if b.Count() != 0 {
		t.Fatalf("Count() = %d after out-of-range Sets, want 0", b.Count())
	}
	if b.Get(-1) || b.Get(8) {
		t.Error("Get out of range returned true")
	}
}
