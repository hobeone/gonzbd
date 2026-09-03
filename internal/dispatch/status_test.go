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
	tests := []struct {
		name string
		view job.RenderView
		want constants.Status
	}{
		{
			name: "Running + Fetching => StatusDownloading",
			view: job.RenderView{Running: true, State: job.Fetching},
			want: constants.StatusDownloading,
		},
		{
			name: "Running + Assessing => StatusVerifying",
			view: job.RenderView{Running: true, State: job.Assessing},
			want: constants.StatusVerifying,
		},
		{
			name: "Running + Repairing => StatusRepairing",
			view: job.RenderView{Running: true, State: job.Repairing},
			want: constants.StatusRepairing,
		},
		{
			name: "Running + Extracting => StatusExtracting",
			view: job.RenderView{Running: true, State: job.Extracting},
			want: constants.StatusExtracting,
		},
		{
			name: "Running + Finishing => StatusMoving",
			view: job.RenderView{Running: true, State: job.Finalizing},
			want: constants.StatusMoving,
		},
		{
			name: "Not Running + Reason NoLease => StatusQueued",
			view: job.RenderView{Running: false, Reason: job.NoLease},
			want: constants.StatusQueued,
		},
		{
			name: "Not Running + Reason JobPaused => StatusPaused",
			view: job.RenderView{Running: false, Reason: job.UserPaused},
			want: constants.StatusPaused,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Row{ID: "a", View: tc.view}
			if got := r.Status(); got != tc.want {
				t.Errorf("Row.Status() = %q, want %q", got, tc.want)
			}
			if got, want := r.Status(), job.ToSABnzbd(tc.view); got != want {
				t.Fatalf("Row.Status() = %q, ToSABnzbd = %q; they must not diverge", got, want)
			}
		})
	}
}
