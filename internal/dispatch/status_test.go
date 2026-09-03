package dispatch

import (
	"testing"

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
