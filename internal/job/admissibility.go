package job

import (
	"fmt"
	"slices"
)

// admissibleAt is the sole owner of "which verdict may an attempt record from
// which position" — sole in the enumerable sense: `git grep -n 'admits(' --
// 'internal/job/*.go' ':!internal/job/*_test.go' | grep -v '// ' | grep -v
// 'func '` returns one production call site, attempt.go's finish. The two
// filters drop this comment's own mention of the name and the declaration
// below; without them the pattern matches four lines and the claim reads as
// false. It replaces two hand-written guards there,
// one of which was wrong through two review rounds while its own comment
// stated the correct rule beside it.
//
// The value is the set of positions that admit the outcome, not a bool per
// pair, because the rows are what carry meaning: OutcomeOK names one state
// because §3.3 gives Finalizing the only Finish(OutcomeOK) completion, and
// OutcomeUnrecoverable names the Correctness zone because D3 defines it as
// "the job never crossed the boundary". A reader can check each row against
// its clause; a matrix of booleans hides which rule produced which cell.
//
// OutcomePending has no row on purpose: it is not a verdict, and finish
// rejects it before reaching here. TestAdmissibleAt_IsTotal asserts the
// absence rather than letting it be an oversight.
var admissibleAt = map[Outcome][]State{
	// §3.3's work-spine table: Extracting's completion is SetNext(Finalizing);
	// Finish(OutcomeOK) appears on Finalizing's row alone. At Extracting the
	// archives are unpacked into the working directory and nothing has been
	// moved to the destination or run a user script, so settling OK there
	// reports a complete job whose output is stranded in a temporary unpack
	// directory.
	//
	// This row is deliberately NOT the Production zone. The zone predicate is
	// too weak here by exactly one state, and it read as correct for two review
	// rounds because the guard it replaced was written when the question was
	// "did this attempt reach Production at all". The consequence of getting it
	// wrong is not local: with no guard, Finish(OutcomeOK) settles an attempt
	// in Fetching, and BeginAttempt then opens a SECOND attempt on a job
	// already declared complete. BeginAttempt cannot stop that on its own —
	// ErrBoundaryConsumed is its one error return, and it is reached only for
	// an attempt that crossed, which an attempt settled in Fetching has not.
	OutcomeOK: {Finalizing},
	// §3.3's failure table: every state either continues to another work state
	// or settles Failed. There is no position from which failing is illegal.
	OutcomeFailed: {Fetching, Assessing, Repairing, Extracting, Finalizing},
	// D3: Unrecoverable means the job never crossed, so its files stay in the
	// working directory and it stays retryable. Past the boundary that is
	// false by construction.
	//
	// This row IS the Correctness zone, and correctly so — D3 defines the rule
	// over Production generally rather than any single state, which is what
	// makes it different in shape from OutcomeOK's row above. It is expressed
	// as the three states rather than as a call to IsCorrectness so that every
	// row of this table reads the same way and a reader compares them without
	// translating between two notations.
	OutcomeUnrecoverable: {Fetching, Assessing, Repairing},
	// Admissible everywhere, including Production. §5.12 forces it: a
	// post-boundary job restored from a restart holds nothing, running(j) is
	// false, and finishCancel settles it Cancelled from Extracting. Refusing
	// that cell would deadlock the scenario revision 3 was fixed to unblock.
	OutcomeCancelled: {Fetching, Assessing, Repairing, Extracting, Finalizing},
}

// admits reports whether o may be recorded by an attempt sitting at s.
func admits(o Outcome, s State) bool {
	return slices.Contains(admissibleAt[o], s)
}

// inadmissible names why, preserving the sentinel each refusal already had.
// ErrUnrecoverableAfterBoundary stays distinct from ErrInvalidOutcome because
// it means something different to a caller: a downstream stage produced a
// verdict that contradicts where the attempt actually got to, which is a
// caller bug to fix rather than a job to fail.
func inadmissible(o Outcome, s State) error {
	if o == OutcomeUnrecoverable {
		return fmt.Errorf("%w: this attempt is at %s", ErrUnrecoverableAfterBoundary, s)
	}
	return fmt.Errorf("%w: %s is not admissible at %s; it is admissible at %v",
		ErrInvalidOutcome, o, s, admissibleAt[o])
}
