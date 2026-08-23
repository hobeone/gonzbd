package durability

import (
	"strings"
	"testing"
)

// TestOverlapFrom_OnlyASurplusIsAFinding pins §3.3's three-way rule directly
// on the classifier, so the arithmetic is fixed by something other than the
// barrier tests that reach it through a fixture.
//
// Only ONE of the three comparisons is a positive signal:
//
//	Σ Length >  size  definite overlap — articles wrote over each other
//	Σ Length == size  no evidence, which is not proof of absence
//	Σ Length <  size  articles are missing: the ordinary incomplete case
//
// The equality row is the one worth pinning explicitly rather than folding
// into "not greater". It is deliberately silent, and a reader who expects a
// complete overlap detector has to be able to see that it is not one: a hole
// of N bytes and an overlap of N bytes cancel and land exactly here.
func TestOverlapFrom_OnlyASurplusIsAFinding(t *testing.T) {
	tests := []struct {
		name     string
		runs     []Run
		size     int64
		wantFind bool
	}{
		{
			name: "surplus is an overlap",
			runs: []Run{{Offset: 0, Length: 200}, {Offset: 150, Length: 50}},
			size: 200, wantFind: true,
		},
		{
			name: "equality is silent",
			runs: []Run{{Offset: 0, Length: 100}, {Offset: 200, Length: 100}},
			size: 200, wantFind: false,
		},
		{
			name: "shortfall is the ordinary incomplete file",
			runs: []Run{{Offset: 0, Length: 100}},
			size: 300, wantFind: false,
		},
		{
			name: "no runs at all is silent",
			runs: nil,
			size: 300, wantFind: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pa, ok := overlapFrom(tt.runs, tt.size, 3, func() string { return "/d/vol042.rar" })
			if ok != tt.wantFind {
				t.Fatalf("overlapFrom found=%v, want %v", ok, tt.wantFind)
			}
			if !ok {
				return
			}
			if pa.FileIdx != 3 {
				t.Errorf("FileIdx = %d, want 3 — Run iterates a job's files internally, "+
					"so its caller cannot tell which file a finding belongs to", pa.FileIdx)
			}
			if !strings.Contains(pa.Reason, "vol042.rar") {
				t.Errorf("Reason = %q, want it to name the file", pa.Reason)
			}
		})
	}
}

// TestOverlapFrom_ResolvesThePathOnlyWhenItReports pins the reason
// overlapFrom takes a closure rather than a string.
//
// Resolving a file's path takes the pipeline's RLock, looks the file up and
// copies a FileInfo, and Go evaluates arguments before the call — so a
// resolved string would pay that on every file of every checkpoint while
// almost every call returns false.
func TestOverlapFrom_ResolvesThePathOnlyWhenItReports(t *testing.T) {
	calls := 0
	pathFn := func() string { calls++; return "/d/f.bin" }

	if _, ok := overlapFrom([]Run{{Offset: 0, Length: 10}}, 100, 0, pathFn); ok {
		t.Fatal("a file with room to spare reported an overlap")
	}
	if calls != 0 {
		t.Errorf("the path was resolved %d times for a file with no finding, want 0 — "+
			"that lock is taken on every file of every checkpoint", calls)
	}

	if _, ok := overlapFrom([]Run{{Offset: 0, Length: 200}}, 100, 0, pathFn); !ok {
		t.Fatal("a surplus did not report")
	}
	if calls != 1 {
		t.Errorf("the path was resolved %d times for one finding, want 1", calls)
	}
}
