package job

import (
	"errors"
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
		if got := i.String(); got == "" || got[0] == 'I' && got == "Intent(" {
			t.Errorf("Intent(%d).String() = %q, which is not a declared arm", uint8(i), got)
		}
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
