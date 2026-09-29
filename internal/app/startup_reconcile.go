package app

import "github.com/hobeone/gonzbd/internal/job"

// startupHandOffs returns the IDs of the complete jobs that Start hands to
// post-processing itself. A job qualifies when it has never run, or is at
// Fetching with no Next recorded. Its download-complete report was lost or
// never made, and the dispatcher keeps it at Fetching until one arrives.
//
// It runs in Dispatcher.StartWith's beforeFirstTick step, after
// resumeAllJobs. No tick has run yet, and Application.Start starts the
// downloader only after StartWith returns. So the state and the completeness
// it reads are what was restored, plus what the resume sweep repaired.
//
// A job at Fetching with Next recorded does not qualify. The first unpaused
// tick moves it to Assessing (Advance's branch 3), and it reaches
// post-processing from there, through runAssess. A hand-off from Start as
// well would run the assess worker beside an admitted post-processing run.
func (app *Application) startupHandOffs() map[string]bool {
	out := make(map[string]bool)
	if app.dispatcher == nil {
		return out
	}
	for _, row := range app.dispatcher.List() {
		v := row.View
		if v.Next != job.StateUnset || (v.State != job.StateUnset && v.State != job.Fetching) {
			continue
		}
		if j, ok := app.dispatcher.Job(row.ID); ok && j.IsComplete() {
			out[row.ID] = true
		}
	}
	return out
}
