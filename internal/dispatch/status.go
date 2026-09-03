package dispatch

import (
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
)

// Status renders this row's state in the legacy SABnzbd vocabulary.
//
// It lives here rather than on job.Job because ToSABnzbd needs a RenderView,
// and three of that view's fields -- Running, Reason and Holds -- are facts
// only internal/sched can supply (job/render.go: "Nothing in this package can
// answer that"). internal/sched imports internal/job and not the reverse, so a
// Status() on Job would need a back-pointer that inverts the dependency into a
// cycle. Row already carries the view.
//
// This is a door onto ToSABnzbd, NOT a second translation: §12 makes that
// function the one place constants.Status may appear, and it is write-only.
// Use this to RENDER a status. Do not branch on the result -- branching is
// what the State/Outcome/Intent axes are for, and reading status back into the
// machine is what the swap exists to end.
func (r Row) Status() constants.Status { return job.ToSABnzbd(r.View) }
