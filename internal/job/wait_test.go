package job

import (
	"strconv"
	"testing"
)

func TestWaitReason_String(t *testing.T) {
	for _, tc := range []struct {
		r    WaitReason
		want string
	}{
		{NoLease, "NoLease"},
		{NoComputeSlot, "NoComputeSlot"},
		{UserPaused, "UserPaused"},
		{GlobalPause, "GlobalPause"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.r.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := WaitReason(9).String(); got != "WaitReason(9)" {
		t.Errorf("String() = %q, want %q", got, "WaitReason(9)")
	}
}

// TestWaitReason_IsPause pins the split that ToSABnzbd depends on: a job held
// for capacity renders as Queued, a job held by a pause renders as Paused. It
// drives off AllWaitReasons() rather than a hand-written table, so a reason
// added without an entry in wantIsPause fails here — the earlier literal
// slice-of-structs table stayed in sync with AllWaitReasons() only by hand.
func TestWaitReason_IsPause(t *testing.T) {
	wantIsPause := map[WaitReason]bool{
		NoLease:       false,
		NoComputeSlot: false,
		UserPaused:    true,
		GlobalPause:   true,
	}
	for _, r := range AllWaitReasons() {
		t.Run(r.String(), func(t *testing.T) {
			want, ok := wantIsPause[r]
			if !ok {
				t.Fatalf("WaitReason %s (%d) has no IsPause expectation declared in wantIsPause; "+
					"a new reason needs an explicit IsPause() decision, not the zero-value false default",
					r, uint8(r))
			}
			if got := r.IsPause(); got != want {
				t.Errorf("IsPause() = %v, want %v", got, want)
			}
		})
	}
}

// TestAllWaitReasons_HaveStringArms fails if a reason is added to
// AllWaitReasons() without a matching arm in String() (falling through to the
// "WaitReason(N)" fallback), or if the declared count drifts from the const
// block. It does not exercise IsPause(); TestWaitReason_IsPause owns that
// coverage by iterating this same enumeration.
func TestAllWaitReasons_HaveStringArms(t *testing.T) {
	for _, r := range AllWaitReasons() {
		if r.String() == "WaitReason("+strconv.Itoa(int(r))+")" {
			t.Errorf("WaitReason(%d) is in AllWaitReasons() but has no String() arm", r)
		}
	}
	if len(AllWaitReasons()) != 4 {
		t.Errorf("AllWaitReasons() has %d entries, expected 4; a new reason needs a String() arm, "+
			"an IsPause() expectation in TestWaitReason_IsPause, and this count updated", len(AllWaitReasons()))
	}
}

func TestStateView_ZeroValueIsUnset(t *testing.T) {
	var v StateView
	if v.State != StateUnset || v.Next != StateUnset || v.Reason != NoLease ||
		v.Activity != ActNone || v.Outcome != OutcomePending {
		t.Errorf("zero StateView = %+v; want State=StateUnset Next=StateUnset Reason=NoLease "+
			"Activity=ActNone Outcome=OutcomePending", v)
	}
}
