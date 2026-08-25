package job

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestToSABnzbd(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    StateView
		want constants.Status
	}{
		{"never run", StateView{State: Waiting, Next: Fetching, Reason: NoLease}, constants.StatusQueued},
		{"waiting for a compute slot", StateView{State: Waiting, Next: Assessing, Reason: NoComputeSlot}, constants.StatusQueued},
		{"user paused", StateView{State: Waiting, Next: Fetching, Reason: UserPaused}, constants.StatusPaused},
		{"globally paused", StateView{State: Waiting, Next: Extracting, Reason: GlobalPause}, constants.StatusPaused},

		{"first-pass download", StateView{State: Fetching}, constants.StatusDownloading},
		{"fetching recovery volumes", StateView{State: Fetching, Assessed: true}, constants.StatusFetching},

		{"cheap verification", StateView{State: Assessing, Activity: ActCRCCheck}, constants.StatusQuickCheck},
		{"full verification", StateView{State: Assessing, Activity: ActPar2Verify}, constants.StatusVerifying},
		{"assessing, no activity yet", StateView{State: Assessing}, constants.StatusVerifying},

		{"repairing", StateView{State: Repairing, Activity: ActPar2Repair}, constants.StatusRepairing},
		{"extracting", StateView{State: Extracting, Activity: ActUnpack}, constants.StatusExtracting},
		{"volume recovery is still extracting", StateView{State: Extracting, Activity: ActVolumeRecovery}, constants.StatusExtracting},

		{"finalizing, moving", StateView{State: Finalizing, Activity: ActMove}, constants.StatusMoving},
		{"finalizing, script", StateView{State: Finalizing, Activity: ActScript}, constants.StatusRunning},
		{"finalizing, cleanup", StateView{State: Finalizing, Activity: ActCleanup}, constants.StatusMoving},

		{"completed", StateView{State: Finished, Outcome: OutcomeOK}, constants.StatusCompleted},
		{"failed", StateView{State: Finished, Outcome: OutcomeFailed}, constants.StatusFailed},
		{"unrecoverable renders as failed", StateView{State: Finished, Outcome: OutcomeUnrecoverable}, constants.StatusFailed},
		{"cancelled renders as deleted", StateView{State: Finished, Outcome: OutcomeCancelled}, constants.StatusDeleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToSABnzbd(tc.v); got != tc.want {
				t.Errorf("ToSABnzbd(%+v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// TestToSABnzbd_IsTotal walks the whole product space of the axes and fails
// if any combination yields the empty status. The shim is a boundary the API
// depends on, and an unhandled combination there is a blank status string in
// a third-party client rather than a crash — which is exactly the kind of
// failure that ships unnoticed.
func TestToSABnzbd_IsTotal(t *testing.T) {
	states, activities, outcomes, reasons := AllStates(), AllActivities(), AllOutcomes(), AllWaitReasons()
	if len(states) == 0 || len(activities) == 0 || len(outcomes) == 0 || len(reasons) == 0 {
		t.Fatalf("one of the enumeration axes is empty: states=%d activities=%d outcomes=%d reasons=%d",
			len(states), len(activities), len(outcomes), len(reasons))
	}
	for _, s := range states {
		for _, a := range activities {
			for _, o := range outcomes {
				for _, r := range reasons {
					for _, assessed := range []bool{false, true} {
						v := StateView{State: s, Activity: a, Outcome: o, Reason: r, Assessed: assessed}
						if got := ToSABnzbd(v); got == "" {
							t.Errorf("ToSABnzbd(%+v) returned the empty status", v)
						}
					}
				}
			}
		}
	}
}

// TestToSABnzbd_EmitsOnlyDeclaredStatuses guards against a typo producing a
// status string no client knows. Every output must be a declared constant.
func TestToSABnzbd_EmitsOnlyDeclaredStatuses(t *testing.T) {
	declared := make(map[constants.Status]bool)
	all := constants.AllStatuses()
	if len(all) == 0 {
		t.Fatal("constants.AllStatuses() is empty")
	}
	for _, s := range all {
		declared[s] = true
	}
	states, activities, outcomes, reasons := AllStates(), AllActivities(), AllOutcomes(), AllWaitReasons()
	if len(states) == 0 || len(activities) == 0 || len(outcomes) == 0 || len(reasons) == 0 {
		t.Fatalf("one of the enumeration axes is empty: states=%d activities=%d outcomes=%d reasons=%d",
			len(states), len(activities), len(outcomes), len(reasons))
	}
	for _, s := range states {
		for _, a := range activities {
			for _, o := range outcomes {
				for _, r := range reasons {
					got := ToSABnzbd(StateView{State: s, Activity: a, Outcome: o, Reason: r})
					if !declared[got] {
						t.Errorf("ToSABnzbd emitted %q, which is not in constants.AllStatuses()", got)
					}
				}
			}
		}
	}
}
