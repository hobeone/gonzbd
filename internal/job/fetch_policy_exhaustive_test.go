package job

import (
	"slices"
	"testing"
)

// TestAllFetchPolicies_Exhaustive pins AllFetchPolicies against the const
// block it mirrors.
//
// AllFetchPolicies' own doc comment already claimed this test closed the loop,
// and repair_state_exhaustive_test.go twice cites "the FetchPolicy equivalent"
// as the sibling it is modelled on. Neither was true: the test did not exist,
// so the hand-written list was the unbacked third copy of the enum that every
// one of those comments said it was not.
//
// What the list is FOR needs stating precisely, because the first version of
// this comment got it wrong in the same way the comments above got their
// claims wrong. AllFetchPolicies has no production caller at all — `git grep
// -nw 'AllFetchPolicies' -- '*.go'` returns only this file, the declaration,
// and two comments. Production never walks the list: it inspects each file's
// Fetch field with the two binary predicates directly, `!= FetchAlways` for
// dispatch and completion and `== FetchIfNeeded` for the deferred-par2 paths
// (content.go's undeferRecovery, DeferredRecoveryIndices and the completion
// scan).
//
// So the list is a registry for exhaustive TESTS, and this test is what keeps
// it honest. The danger a missing entry creates is real but indirect: a policy
// declared in progress.go and absent here is invisible to every table driven
// from the list, so a case nobody considered reads as covered. The predicates
// themselves would still see the new value — and treat it as whichever side of
// each binary they happen to fall on, which is the decision the const block's
// own comment says must be made deliberately at each site.
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

	all := AllFetchPolicies()
	for name, value := range declared {
		if !slices.Contains(all, FetchPolicy(value)) {
			t.Errorf("%s is declared in progress.go but missing from AllFetchPolicies(); add it there, "+
				"then decide what it means at each site that reads the policy — the fetch/hold predicates "+
				"that drive CRC re-verification and whether a late failure may re-arm a volume", name)
		}
	}
	if len(all) != len(declared) {
		t.Errorf("AllFetchPolicies() has %d entries, progress.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(all), len(declared))
	}
}
