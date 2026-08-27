package job

import (
	"slices"
	"testing"
)

// ErrFinishRequired's doc comment claims finish is the only mutator that
// assigns Outcome. That is a claim about the whole package's population of
// assignments to the unexported outcome field, not just this file's — and
// job.go has access to the same unexported field, so the claim can be
// falsified by a file this one never opens.
//
// This makes the enumeration a fact the compiler's own package boundary can
// check, rather than a sentence kept true by memory. It follows the pattern
// in internal/queue/donebit_enumeration_test.go
// (TestDoneBitWriters_MatchTheEnumerationStatedInProse, cited in AGENTS.md).
// The scan itself — scanWriters, in writer_enumeration_test.go — is shared
// with five other fields now; only the field name and the wanted set are
// specific to this test.
//
// The test asserts the enumeration (the function names), not a bare count. A
// count alone would go green against a write that moved from finish to some
// other function, which is exactly the drift this exists to catch.

// outcomeWriters is every function in this package that sets the unexported
// outcome field, via a plain or compound `=` assignment (`a.outcome = x`,
// `a.outcome += x`), `a.outcome++`/`--`, or a keyed `outcome: x` element in a
// composite literal whose type is Attempt. `:=` declarations and `==`
// comparisons are not assignments to an existing field and are not counted.
// An unkeyed Attempt{...} literal fails the test outright rather than being
// silently miscounted — see scanWriters' CompositeLit case.
// An aliased write — `b := a; b.outcome = OutcomeOK` — IS counted: the scan
// matches on the selector's field name, so the receiver's spelling does not
// matter. What it cannot do is confirm that receiver is an Attempt, which is
// why fieldOwner exists and why TestFieldOwners_AreTheOnlyDeclarers asserts no
// other package struct declares one of these names. Not covered: reflection.
var outcomeWriters = []string{"finish"}

func TestOutcomeWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	writers := scanWriters(t, "outcome")
	if !slices.Equal(writers, outcomeWriters) {
		t.Errorf("functions assigning outcome = %v, want %v\n\n"+
			"ErrFinishRequired's doc comment claims finish is the only "+
			"mutator that assigns Outcome. If a second writer is correct, "+
			"say so at that comment AND update this list together — a "+
			"comment that still says \"the only mutator\" once a second one "+
			"exists is worse than no comment at all.",
			writers, outcomeWriters)
	}
}
