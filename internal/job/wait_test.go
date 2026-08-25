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
// for capacity renders as Queued, a job held by a pause renders as Paused.
func TestWaitReason_IsPause(t *testing.T) {
	for _, tc := range []struct {
		r    WaitReason
		want bool
	}{
		{NoLease, false},
		{NoComputeSlot, false},
		{UserPaused, true},
		{GlobalPause, true},
	} {
		t.Run(tc.r.String(), func(t *testing.T) {
			if got := tc.r.IsPause(); got != tc.want {
				t.Errorf("IsPause() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAllWaitReasons_CoveredByIsPause fails if a reason is added without
// IsPause being taught about it. A new reason defaults to false there, which
// would silently render a paused job as Queued; this makes that a test
// failure rather than a UI bug.
func TestAllWaitReasons_CoveredByIsPause(t *testing.T) {
	for _, r := range AllWaitReasons() {
		if r.String() == "WaitReason("+strconv.Itoa(int(r))+")" {
			t.Errorf("WaitReason(%d) is in AllWaitReasons() but has no String() arm", r)
		}
	}
	if len(AllWaitReasons()) != 4 {
		t.Errorf("AllWaitReasons() has %d entries, expected 4; a new reason needs a String() arm, "+
			"an IsPause() decision, and a row in TestWaitReason_IsPause", len(AllWaitReasons()))
	}
}

func TestStateView_ZeroValueIsWaitingForALease(t *testing.T) {
	var v StateView
	if v.State != Waiting || v.Next != Waiting || v.Reason != NoLease {
		t.Errorf("zero StateView = %+v; want State=Waiting Next=Waiting Reason=NoLease", v)
	}
}
