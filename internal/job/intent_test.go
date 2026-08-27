package job

import (
	"errors"
	"strconv"
	"testing"
)

func TestIntent_RunIsZero(t *testing.T) {
	var zero Intent
	if zero != IntentRun {
		t.Errorf("zero Intent = %v, want IntentRun", zero)
	}
}

func TestAllIntents_EveryEntryHasAStringArm(t *testing.T) {
	all := AllIntents()
	if len(all) == 0 {
		t.Fatal("AllIntents() is empty")
	}
	for _, i := range all {
		// Compare against the exact fallback String() would produce for this
		// value. The earlier form — `got == "" || got[0] == 'I' && got ==
		// "Intent("` — asserted nothing: && binds tighter than ||, and no
		// value can render as the bare prefix "Intent(", so the whole
		// condition collapsed to `got == ""`, which String() never returns.
		// Deleting IntentPause's arm left that version green. This is the
		// idiom the three sibling enum tests in this package already use
		// (outcome_test.go, activity_test.go, wait_test.go).
		if got := i.String(); got == "Intent("+strconv.Itoa(int(i))+")" {
			t.Errorf("Intent(%d) is in AllIntents() but falls to the default String() arm (%q); "+
				"every declared intent needs its own case", uint8(i), got)
		}
	}
	if len(all) != 3 {
		t.Errorf("AllIntents() has %d entries, expected 3; a new intent needs a String() arm, "+
			"a row in TestIntent_String, an IsLatched() decision, and this count updated", len(all))
	}
	if got := Intent(200).String(); got != "Intent(200)" {
		t.Errorf("Intent(200).String() = %q, want the fallback form", got)
	}
}

// TestJob_IntentDefaultsToRun pins that a fresh job is not gated by anything
// it never asked for.
func TestJob_IntentDefaultsToRun(t *testing.T) {
	j := newTestJob(t)
	if got := j.Intent(); got != IntentRun {
		t.Errorf("Intent() on a fresh job = %v, want IntentRun", got)
	}
}

// TestJob_IntentRunAndPauseAreReversible pins §3.1: only cancel latches.
func TestJob_IntentRunAndPauseAreReversible(t *testing.T) {
	j := newTestJob(t)
	for _, want := range []Intent{IntentPause, IntentRun, IntentPause, IntentRun} {
		if err := j.SetIntent(want); err != nil {
			t.Fatalf("SetIntent(%v): %v", want, err)
		}
		if got := j.Intent(); got != want {
			t.Errorf("Intent() = %v, want %v", got, want)
		}
	}
}

// TestJob_IntentCancelLatches pins the one restriction §3.1 places on
// transitions: cancel is final for this Job. Prior spec D8 makes a full redo a
// re-added NZB starting a NEW Job, so clearing the latch would let a job the
// user deleted come back through a path that never re-asked them.
func TestJob_IntentCancelLatches(t *testing.T) {
	j := newTestJob(t)
	if err := j.SetIntent(IntentCancel); err != nil {
		t.Fatalf("SetIntent(IntentCancel): %v", err)
	}
	for _, try := range []Intent{IntentRun, IntentPause} {
		if err := j.SetIntent(try); !errors.Is(err, ErrIntentLatched) {
			t.Errorf("SetIntent(%v) after cancel, error = %v, want ErrIntentLatched", try, err)
		}
		if got := j.Intent(); got != IntentCancel {
			t.Fatalf("Intent() = %v after a refused SetIntent; the refusal must not have partially applied", got)
		}
	}
	// Re-asserting cancel is a no-op, not an error: an idempotent cancel from a
	// retrying caller is not a mistake to report.
	if err := j.SetIntent(IntentCancel); err != nil {
		t.Errorf("SetIntent(IntentCancel) twice = %v, want nil", err)
	}
}

// TestJob_SetIntentIsLegalInEveryState pins §3.1's correction to revision 3,
// which refused SetIntent on a settled attempt by analogy with wait reasons. An
// intent is not a wait reason: a settled job may be retried, and the intent it
// carries governs what happens when it is. Refusing left a job that was paused
// and then failed neither unpausable nor usefully retriable.
func TestJob_SetIntentIsLegalInEveryState(t *testing.T) {
	j := newTestJob(t)
	if err := j.SetIntent(IntentPause); err != nil {
		t.Fatalf("SetIntent on a never-run job: %v", err)
	}
	mustBegin(t, j)
	if err := j.SetIntent(IntentRun); err != nil {
		t.Fatalf("SetIntent on an open attempt: %v", err)
	}
	if _, err := j.Finish(OutcomeFailed, testClock()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := j.SetIntent(IntentPause); err != nil {
		t.Fatalf("SetIntent on a SETTLED attempt: %v — a settled job may be retried, and its intent governs that retry", err)
	}
}

// TestAllIntents_Exhaustive fails when intent.go declares an Intent that
// AllIntents() does not list. The count check in TestAllIntents_HaveStringArms
// cannot do this: it compares AllIntents() against a number, so a constant
// added to intent.go and forgotten in AllIntents() leaves both in agreement at
// three and every table driven by the enumeration silently stops covering it.
// State has had this enforcement since TestAllStates_Exhaustive; Intent did
// not, and the asymmetry is the whole finding.
func TestAllIntents_Exhaustive(t *testing.T) {
	declared := constantsOfType(t, "intent.go", "Intent")
	if len(declared) == 0 {
		t.Fatal("parsed no Intent constants from intent.go; the walk no longer matches the file's shape, so this test would pass vacuously")
	}

	listed := make(map[string]bool, len(AllIntents()))
	for _, in := range AllIntents() {
		listed[in.String()] = true
	}
	for name := range declared {
		if !listed[name] {
			t.Errorf("%s is declared in intent.go but missing from AllIntents(); add it there, give it a String() arm and an IsLatched() decision", name)
		}
	}
	if len(AllIntents()) != len(declared) {
		t.Errorf("AllIntents() has %d entries, intent.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(AllIntents()), len(declared))
	}
}

// TestIntent_String was cited by TestAllIntents_EveryEntryHasAStringArm's
// failure text — "a new intent needs ... a row in TestIntent_String" — before
// it existed. Every sibling enum has one (TestState_String, TestActivity_String,
// TestOutcome_String, TestWaitReason_String); Intent's exact renderings were
// asserted nowhere, while the arm-coverage test only checked that each value
// avoided the default arm.
func TestIntent_String(t *testing.T) {
	for _, tc := range []struct {
		i    Intent
		want string
	}{
		{IntentRun, "IntentRun"},
		{IntentPause, "IntentPause"},
		{IntentCancel, "IntentCancel"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.i.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
	if want := len(AllIntents()); want != 3 {
		t.Errorf("this table has 3 rows but AllIntents() has %d; add the missing row", want)
	}
}

// TestJob_SetIntentRejectsUndeclared pins the membership check. An undeclared
// value stored in j.intent is not cosmetic: IsLatched() is false for it, so the
// cancel latch never engages and every gate comparing against IntentPause or
// IntentCancel reads the job as un-gated.
func TestJob_SetIntentRejectsUndeclared(t *testing.T) {
	j := newTestJob(t)
	if err := j.SetIntent(Intent(99)); !errors.Is(err, ErrInvalidIntent) {
		t.Fatalf("SetIntent(Intent(99)) = %v, want ErrInvalidIntent", err)
	}
	if got := j.Intent(); got != IntentRun {
		t.Errorf("Intent() = %v after a rejected SetIntent, want IntentRun unchanged", got)
	}
	for _, i := range AllIntents() {
		j := newTestJob(t)
		if err := j.SetIntent(i); err != nil {
			t.Errorf("SetIntent(%v) = %v, want nil — every declared intent must be accepted", i, err)
		}
	}
}
