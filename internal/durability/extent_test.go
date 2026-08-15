package durability

import (
	"bytes"
	"errors"
	"testing"
)

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

// TestBitmapFromBytes_MasksPaddingBits pins that garbage in the final
// word's padding bits (beyond n, at a non-multiple-of-64 n) is not
// absorbed into Count. Get is bounded by n, but without a mask on the
// final word, Count is not — a damaged persisted buffer could then report
// more durable articles than the file actually has, which is the
// over-claim direction the design forbids.
func TestBitmapFromBytes_MasksPaddingBits(t *testing.T) {
	buf := bytes.Repeat([]byte{0xFF}, 32) // 256 bits, all set
	got, err := BitmapFromBytes(buf, 200)
	if err != nil {
		t.Fatalf("BitmapFromBytes: %v", err)
	}
	if got.Count() != 200 {
		t.Fatalf("Count() = %d, want 200 (padding bits leaked in)", got.Count())
	}
	if got.Get(200) {
		t.Error("Get(200) = true, want false: bit is beyond Len()")
	}
}

// TestBitmapFromBytes_RejectsShortBuffer pins that a truncated persisted
// bitmap is an error rather than a silently short map. S3 requires that an
// article whose state cannot be established is Outstanding; a silently
// truncated bitmap would instead report those articles as not-durable
// without anyone noticing the record was damaged.
func TestBitmapFromBytes_RejectsShortBuffer(t *testing.T) {
	_, err := BitmapFromBytes([]byte{0x01}, 200)
	if !errors.Is(err, ErrBitmapTooShort) {
		t.Fatalf("BitmapFromBytes error = %v, want ErrBitmapTooShort", err)
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

// TestBitmap_SetMutatesCopyThroughSharedBackingArray pins the doc comment's
// load-bearing claim on Set: despite the value receiver, Set mutates the
// caller's Bitmap because words is a slice and a copy of the struct shares
// its backing array. A caller that copies a Bitmap and calls Set on the
// copy must still see the bit on the original.
func TestBitmap_SetMutatesCopyThroughSharedBackingArray(t *testing.T) {
	orig := NewBitmap(8)
	cp := orig
	cp.Set(3)
	if !orig.Get(3) {
		t.Fatal("Set on a copied Bitmap did not mutate the original")
	}
}

// TestFileExtent_DurableBitmapSetMutatesThroughCopy pins the same aliasing
// behaviour through FileExtent, which holds Durable Bitmap by value.
// Tasks 6 and 7 pass FileExtent values around by copy and call Set on
// those copies; if Set ever became a pointer-receiver method, this is the
// exact case that would silently stop working.
func TestFileExtent_DurableBitmapSetMutatesThroughCopy(t *testing.T) {
	orig := FileExtent{FileIdx: 1, Durable: NewBitmap(8)}
	cp := orig
	cp.Durable.Set(5)
	if !orig.Durable.Get(5) {
		t.Fatal("Set on a copied FileExtent's Durable bitmap did not mutate the original")
	}
}
