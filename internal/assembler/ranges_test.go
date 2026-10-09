package assembler

import "testing"

func TestOwnedRanges_IntersectionIsOwned(t *testing.T) {
	var o ownedRanges
	a := articleID{artIdx: 1, msgID: "<a@x>"}
	b := articleID{artIdx: 2, msgID: "<b@x>"}
	o.claim(Range{Off: 0, Len: 1000}, a)

	cases := []struct {
		name string
		r    Range
		want bool
	}{
		{"same start", Range{0, 10}, true},
		{"straddles end", Range{500, 1000}, true},
		{"abuts end", Range{1000, 10}, false},
		{"zero-length inside", Range{10, 0}, false},
		{"before", Range{-10, 10}, false},
	}
	for _, tc := range cases {
		if _, got := o.ownerOf(tc.r, b); got != tc.want {
			t.Errorf("%s: ownerOf(%+v) owned = %v, want %v", tc.name, tc.r, got, tc.want)
		}
	}
	if _, got := o.ownerOf(Range{0, 1000}, a); got {
		t.Error("an article does not collide with its own range (a write-fault retry re-arrives at the same offset)")
	}
}

func TestOwnedRanges_SeededRangeIsOwnedByEveryArrival(t *testing.T) {
	var o ownedRanges
	o.seed([]Range{{0, 100}})
	// artIdx 0 is the zero articleID's index: a zero sentinel would treat it
	// as the owner itself and wave it through.
	if _, got := o.ownerOf(Range{50, 10}, articleID{artIdx: 0}); !got {
		t.Error("article 0 was waved through a seeded range")
	}
}

func TestOwnedRanges_ClaimKeepsOffsetOrder(t *testing.T) {
	var o ownedRanges
	for i, off := range []int64{200, 0, 100} {
		o.claim(Range{off, 100}, articleID{artIdx: int32(i)})
	}
	// A probe in the middle of the slice must find the right entry, which a
	// slice left unsorted by claim would miss.
	if got, ok := o.ownerOf(Range{150, 10}, articleID{artIdx: 9}); !ok || got.artIdx != 2 {
		t.Errorf("ownerOf([150,160)) = %+v, %v; want article 2", got, ok)
	}
	o.claim(Range{100, 100}, articleID{artIdx: 7})
	if got, _ := o.ownerOf(Range{150, 10}, articleID{artIdx: 9}); got.artIdx != 7 || len(o.s) != 3 {
		t.Errorf("re-claim: owner %+v, len %d; want article 7 replacing in place", got, len(o.s))
	}
}
