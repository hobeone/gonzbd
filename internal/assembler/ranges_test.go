package assembler

import (
	"errors"
	"testing"
)

func TestOwnedRanges_SeedMergesIntersectingRanges(t *testing.T) {
	probe := articleID{artIdx: 3}
	for _, tc := range []struct {
		name    string
		in      []Range
		covered []Range
		free    []Range
	}{
		{"intersecting", []Range{{0, 100}, {50, 100}}, []Range{{10, 5}, {120, 5}}, []Range{{150, 5}}},
		{"contained", []Range{{0, 100}, {10, 10}}, []Range{{10, 5}, {90, 5}}, []Range{{100, 5}}},
		{"unsorted and abutting", []Range{{200, 50}, {100, 100}, {0, 50}}, []Range{{0, 5}, {150, 5}, {240, 5}}, []Range{{50, 50}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o ownedRanges
			if err := o.seed(tc.in); err != nil {
				t.Fatal(err)
			}
			for _, r := range tc.covered {
				if _, ok := o.ownerOf(r, probe); !ok {
					t.Errorf("ownerOf(%+v) not owned after seeding %+v", r, tc.in)
				}
			}
			for _, r := range tc.free {
				if _, ok := o.ownerOf(r, probe); ok {
					t.Errorf("ownerOf(%+v) owned after seeding %+v", r, tc.in)
				}
			}
		})
	}
}

func TestOwnedRanges_SeedOnNonEmptySetChangesNothing(t *testing.T) {
	var o ownedRanges
	o.claim(Range{0, 10}, articleID{artIdx: 1})
	if err := o.seed([]Range{{100, 10}}); !errors.Is(err, errSeedNotEmpty) {
		t.Fatalf("seed err = %v, want errSeedNotEmpty", err)
	}
	if len(o.s) != 1 || o.s[0].r != (Range{0, 10}) {
		t.Errorf("set = %+v, want it unchanged", o.s)
	}
}

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
	if err := o.seed([]Range{{0, 100}}); err != nil {
		t.Fatal(err)
	}
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
}

func TestOwnedRanges_ReclaimBySameArticleLeavesOneEntry(t *testing.T) {
	a := articleID{artIdx: 1, msgID: "<a@x>"}
	probe := articleID{artIdx: 9}
	for _, tc := range []struct {
		name   string
		second Range
		probe  Range
	}{
		{"longer", Range{0, 100}, Range{60, 10}},
		{"shorter", Range{0, 20}, Range{5, 10}},
		{"shifted", Range{10, 50}, Range{55, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o ownedRanges
			o.claim(Range{200, 50}, articleID{artIdx: 2})
			o.claim(Range{0, 50}, a)
			o.claim(tc.second, a)
			if len(o.s) != 2 {
				t.Fatalf("entries = %+v, want the re-claim to replace the first range", o.s)
			}
			// The probe lies inside the new range; the old range's end would
			// put it past the broken search's starting point.
			if got, ok := o.ownerOf(tc.probe, probe); !ok || got != a {
				t.Errorf("ownerOf(%+v) = %+v, %v; want article 1", tc.probe, got, ok)
			}
			if got, ok := o.ownerOf(Range{210, 10}, probe); !ok || got.artIdx != 2 {
				t.Errorf("the neighbour was lost: ownerOf = %+v, %v", got, ok)
			}
		})
	}
}

func TestOwnedRanges_ClaimOverAnotherArticlePanics(t *testing.T) {
	var o ownedRanges
	o.claim(Range{0, 50}, articleID{artIdx: 1})
	defer func() {
		if recover() == nil {
			t.Error("claiming over another article's range did not panic")
		}
	}()
	o.claim(Range{40, 50}, articleID{artIdx: 2})
}
