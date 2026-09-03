package dispatch

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestRowStatus_MatchesToSABnzbd pins that the accessor is a door onto
// ToSABnzbd rather than a second translation. §12 makes ToSABnzbd the one
// place constants.Status may appear; a Row.Status that computed anything
// itself would be a second enforcement point for that rule (Rule 2).
func TestRowStatus_MatchesToSABnzbd(t *testing.T) {
	for _, st := range job.AllStates() {
		t.Run(st.String(), func(t *testing.T) {
			v := job.RenderView{StateView: job.StateView{State: st}, Running: true}
			r := Row{ID: "a", View: v}
			if got, want := r.Status(), job.ToSABnzbd(v); got != want {
				t.Fatalf("Row.Status() = %q, ToSABnzbd = %q; they must not diverge", got, want)
			}
		})
	}
}

// TestRowStatus_FetchingNotRunningIsQueuedNotDownloading is the concrete
// case TestRowStatus_MatchesToSABnzbd cannot catch: that test compares
// Row.Status against ToSABnzbd for every State with Running always true, so
// a ToSABnzbd that wrongly rendered Fetching as StatusDownloading regardless
// of Running would pass it. sabnzbd.go keys StatusDownloading/StatusFetching
// on State only inside the `Running` branch (sabnzbd.go:45-60) — with
// Running false, ToSABnzbd returns before that switch is ever reached, off
// the wait-reason branch instead. A RenderView at Fetching with Running left
// false (so Reason is the zero WaitReason, NoLease, which is not a pause
// reason) must therefore render StatusQueued.
func TestRowStatus_FetchingNotRunningIsQueuedNotDownloading(t *testing.T) {
	v := job.RenderView{StateView: job.StateView{State: job.Fetching}}
	r := Row{ID: "a", View: v}
	if got := r.Status(); got != constants.StatusQueued {
		t.Fatalf("Row.Status() = %q, want %q (Running=false must not read as StatusDownloading)", got, constants.StatusQueued)
	}
}
