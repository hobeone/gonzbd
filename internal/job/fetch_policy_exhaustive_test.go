package job

import "testing"

// TestAllFetchPolicies_Exhaustive pins AllFetchPolicies against the const
// block it mirrors.
//
// AllFetchPolicies' own doc comment already claimed this test closed the loop,
// and repair_state_exhaustive_test.go twice cites "the FetchPolicy equivalent"
// as the sibling it is modelled on. Neither was true: the test did not exist,
// so the hand-written list was the unbacked third copy of the enum that every
// one of those comments said it was not.
//
// The gap it leaves is silent rather than loud. AllFetchPolicies is what the
// aggregates and the un-defer path walk, so a policy declared in progress.go
// but absent from the list is excluded from every aggregate and invisible to
// un-deferral — its file is never fetched and never blocks completion, and no
// switch falls through to announce it.
//
// progress.go is the source of truth; the list only has to agree with it.
// Compared by iota VALUE rather than by name, because FetchPolicy has no
// String() method to key on — unlike Intent, whose equivalent test can.
func TestAllFetchPolicies_Exhaustive(t *testing.T) {
	t.Parallel()

	declared := constantsOfType(t, "progress.go", "FetchPolicy")
	if len(declared) == 0 {
		t.Fatal("parsed no FetchPolicy constants from progress.go; the walk no longer matches the file's shape, so this test would pass vacuously")
	}

	listed := make(map[int]bool, len(AllFetchPolicies()))
	for _, p := range AllFetchPolicies() {
		listed[int(p)] = true
	}

	for name, value := range declared {
		if !listed[value] {
			t.Errorf("%s is declared in progress.go but missing from AllFetchPolicies(); add it there, "+
				"then decide what it means at each site that reads the policy — the fetch/hold predicates "+
				"that drive CRC re-verification and whether a late failure may re-arm a volume", name)
		}
	}
	if len(AllFetchPolicies()) != len(declared) {
		t.Errorf("AllFetchPolicies() has %d entries, progress.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(AllFetchPolicies()), len(declared))
	}
}
