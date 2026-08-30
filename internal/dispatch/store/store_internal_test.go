package store

import (
	"math"
	"strings"
	"testing"
)

// TestNarrowAll_RejectsOutOfRange exercises the conversion guard directly,
// across the boundary rather than only at one value. store_test.go proves the
// guard fires on a real corrupt row; this proves it fires exactly at the edge.
func TestNarrowAll_RejectsOutOfRange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vs      []int
		wantErr bool
	}{
		{"zero", []int{0}, false},
		{"max uint8 is admissible", []int{math.MaxUint8}, false},
		{"one past max uint8", []int{math.MaxUint8 + 1}, true},
		{"negative", []int{-1}, true},
		{"a bad value among good ones", []int{1, 2, 300, 3}, true},
		{"several good ones", []int{1, 2, 3, 4, 5}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := narrowAll("j1", tc.vs...)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("narrowAll(%v) = %v, nil; want an error", tc.vs, got)
				}
				if !strings.Contains(err.Error(), "j1") {
					t.Errorf("error %q does not name the job, so a failing load cannot say which row", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("narrowAll(%v): %v", tc.vs, err)
			}
			if len(got) != len(tc.vs) {
				t.Fatalf("narrowAll returned %d values, want %d", len(got), len(tc.vs))
			}
			for i, v := range tc.vs {
				if int(got[i]) != v {
					t.Errorf("value %d round-tripped as %d, want %d", i, got[i], v)
				}
			}
		})
	}
}

func TestB2i(t *testing.T) {
	if got := b2i(true); got != 1 {
		t.Errorf("b2i(true) = %d, want 1", got)
	}
	if got := b2i(false); got != 0 {
		t.Errorf("b2i(false) = %d, want 0", got)
	}
}
