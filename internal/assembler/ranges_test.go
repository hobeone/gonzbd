package assembler

import (
	"errors"
	"slices"
	"testing"
)

func TestOwnedRanges_SeedMergesIntersectingRanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      []Range
		covered []Range
		free    []Range
	}{
		{"intersecting", []Range{{0, 100}, {50, 100}}, []Range{{10, 5}, {120, 5}}, []Range{{150, 5}}},
		{"contained", []Range{{0, 100}, {10, 10}}, []Range{{10, 5}, {90, 5}}, []Range{{100, 5}}},
		{"unsorted and abutting", []Range{{200, 50}, {100, 100}, {0, 50}}, []Range{{0, 5}, {150, 5}, {240, 5}}, []Range{{50, 50}}},
		// Kept, {50,-45} would end at 5, before {0,10}'s end, and ownerOf's
		// binary search over ends would skip [0,10) for a probe at 5.
		{"empty and negative lengths dropped", []Range{{0, 10}, {50, -45}, {60, 10}, {30, 0}}, []Range{{5, 3}, {0, 5}, {60, 5}}, []Range{{30, 1}, {40, 10}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o ownedRanges
			if err := o.seed(tc.in); err != nil {
				t.Fatal(err)
			}
			for _, r := range tc.covered {
				if _, ok := o.ownerOf(r); !ok {
					t.Errorf("ownerOf(%+v) not owned after seeding %+v", r, tc.in)
				}
			}
			for _, r := range tc.free {
				if _, ok := o.ownerOf(r); ok {
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
	o.claim(Range{Off: 0, Len: 1000}, articleID{artIdx: 1})

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
		{"1-byte overlap at tail", Range{999, 10}, true},
		{"contained", Range{100, 50}, true},
		{"enclosing", Range{-10, 2000}, true},
		{"disjoint after", Range{2000, 10}, false},
		{"zero-length at start", Range{0, 0}, false},
	}
	for _, tc := range cases {
		if _, got := o.ownerOf(tc.r); got != tc.want {
			t.Errorf("%s: ownerOf(%+v) owned = %v, want %v", tc.name, tc.r, got, tc.want)
		}
	}
}

func TestOwnedRanges_ClaimKeepsOffsetOrder(t *testing.T) {
	var o ownedRanges
	for i, off := range []int64{200, 0, 100} {
		o.claim(Range{off, 100}, articleID{artIdx: int32(i)})
	}
	// A probe inside each range must find its owner, which ownerOf's binary
	// search misses for some entry of a slice claim left unsorted.
	for _, tc := range []struct {
		off  int64
		want int32
	}{{50, 1}, {150, 2}, {250, 0}} {
		if got, ok := o.ownerOf(Range{tc.off, 10}); !ok || got.artIdx != tc.want {
			t.Errorf("ownerOf([%d,%d)) = %+v, %v; want article %d", tc.off, tc.off+10, got, ok, tc.want)
		}
	}
}

// TestOwnedRanges_ClaimOverAnOwnedRangePanics pins that claim refuses every
// intersection, its own owner's included: no owner of a range reaches claim
// again (TestFileWriter_RangeOwnersStayAdmitted), so a claim over one is an
// invariant violation and must not quietly replace the range.
func TestOwnedRanges_ClaimOverAnOwnedRangePanics(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Range
		id   articleID
	}{
		{"another article over the tail", Range{40, 50}, articleID{artIdx: 2}},
		{"the owner itself", Range{0, 50}, articleID{artIdx: 1}},
		{"a range starting below and ending inside", Range{80, 30}, articleID{artIdx: 2}},
		{"a range enclosing two owners", Range{-10, 300}, articleID{artIdx: 2}},
		{"the seeded range", Range{205, 5}, articleID{artIdx: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o ownedRanges
			if err := o.seed([]Range{{200, 50}}); err != nil {
				t.Fatal(err)
			}
			o.claim(Range{0, 50}, articleID{artIdx: 1})
			o.claim(Range{100, 50}, articleID{artIdx: 3})
			defer func() {
				if recover() == nil {
					t.Errorf("claim of %+v by %+v did not panic; set = %+v", tc.r, tc.id, o.s)
				}
			}()
			o.claim(tc.r, tc.id)
		})
	}
}

// TestOwnedRanges_ClaimBetweenAbuttingRangesIsAccepted is the other side of
// the panic: a range that only abuts its neighbours intersects nothing.
func TestOwnedRanges_ClaimBetweenAbuttingRangesIsAccepted(t *testing.T) {
	var o ownedRanges
	o.claim(Range{0, 50}, articleID{artIdx: 1})
	o.claim(Range{100, 50}, articleID{artIdx: 3})
	o.claim(Range{50, 50}, articleID{artIdx: 2})
	want := []Range{{0, 50}, {50, 50}, {100, 50}}
	got := make([]Range, 0, len(o.s))
	for _, e := range o.s {
		got = append(got, e.r)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ranges = %v, want %v", got, want)
	}
}
