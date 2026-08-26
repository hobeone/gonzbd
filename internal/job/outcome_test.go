package job

import (
	"strconv"
	"testing"
)

func TestOutcome_String(t *testing.T) {
	for _, tc := range []struct {
		o    Outcome
		want string
	}{
		{OutcomePending, "Pending"},
		{OutcomeOK, "OK"},
		{OutcomeFailed, "Failed"},
		{OutcomeUnrecoverable, "Unrecoverable"},
		{OutcomeCancelled, "Cancelled"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.o.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := Outcome(42).String(); got != "Outcome(42)" {
		t.Errorf("String() = %q, want %q", got, "Outcome(42)")
	}
}

// TestOutcome_PendingIsZeroAndNotSettled pins the property the write-once
// rule rests on: an attempt that has not reached Finished carries the zero
// Outcome, and the zero Outcome is not a verdict.
func TestOutcome_PendingIsZeroAndNotSettled(t *testing.T) {
	var o Outcome
	if o != OutcomePending {
		t.Fatalf("zero Outcome = %v, want OutcomePending", o)
	}
	if o.IsSettled() {
		t.Error("OutcomePending.IsSettled() = true, want false; a pending attempt has no verdict yet")
	}
}

func TestOutcome_IsSettled(t *testing.T) {
	for _, tc := range []struct {
		o    Outcome
		want bool
	}{
		{OutcomePending, false},
		{OutcomeOK, true},
		{OutcomeFailed, true},
		{OutcomeUnrecoverable, true},
		{OutcomeCancelled, true},
	} {
		t.Run(tc.o.String(), func(t *testing.T) {
			if got := tc.o.IsSettled(); got != tc.want {
				t.Errorf("IsSettled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllOutcomes_EveryEntryHasAStringArm(t *testing.T) {
	for _, o := range AllOutcomes() {
		if got := o.String(); got == "Outcome("+strconv.Itoa(int(o))+")" {
			t.Errorf("Outcome(%d) is in AllOutcomes() but falls to the default String() arm", o)
		}
	}
	if len(AllOutcomes()) != 5 {
		t.Errorf("AllOutcomes() has %d entries, expected 5; a new outcome needs a String() arm, "+
			"a row in TestOutcome_String, an IsSettled() case, and this count updated", len(AllOutcomes()))
	}
}
