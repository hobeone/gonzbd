package app

import "testing"

// TestPar2Outcome_String pins the string each declared outcome renders as.
//
// allPar2Outcomes() and TestAllPar2Outcomes_Exhaustive (in
// par2_outcome_exhaustive_test.go) already pin that the const block and the
// list agree, but nothing called String() itself, so its switch had 0%
// coverage. This test calls it directly for every declared value and checks
// the exact rendering, including the unreachable-in-production default arm.
func TestPar2Outcome_String(t *testing.T) {
	t.Parallel()

	cases := []struct {
		outcome par2Outcome
		want    string
	}{
		{outcomeClean, "clean"},
		{outcomeRepair, "repair"},
		{outcomeUnknown, "unknown"},
		{par2Outcome(99), "invalid"},
	}

	for _, tc := range cases {
		if got := tc.outcome.String(); got != tc.want {
			t.Errorf("par2Outcome(%d).String() = %q, want %q", tc.outcome, got, tc.want)
		}
	}

	// Cross-check against allPar2Outcomes() so a fourth declared outcome
	// without a matching case above is caught here too, rather than only by
	// the exhaustiveness test parsing app.go's const block.
	if got := len(allPar2Outcomes()); got != 3 {
		t.Fatalf("allPar2Outcomes() has %d entries; this test's declared-outcome cases must be updated to match", got)
	}
}
