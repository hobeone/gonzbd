package app

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// stallRecheckInterval is R19's "re-evaluated on an interval".
//
// It measures how long a user waits after clearing a full disk before their
// download moves again, and thirty seconds is short enough not to feel stuck
// while leaving a wedged mount alone most of the time — every re-evaluation of
// a still-broken condition costs a re-fault.
const stallRecheckInterval = 30 * time.Second

// stallRecord is one job's parked state: why it stopped, and whether this
// application paused it.
type stallRecord struct {
	// reason is the rendered, surfaced text, not the fault it came from.
	reason string
	since  time.Time
	// parked records that THIS application paused the job: Stall called
	// PauseJob on a job whose intent was not already IntentPause. A job the
	// user had paused when Stall fired is not marked, so a re-evaluation
	// clears the reason and leaves that pause in place. If the user pauses a
	// job while Stall's pause stands, the record stays parked, and a
	// re-evaluation resumes it.
	parked bool
}

// stalledJobIDs returns the parked jobs in a stable order.
func (app *Application) stalledJobIDs() []string {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	return slices.Sorted(maps.Keys(app.stalls))
}

// noteStall records why a job is parked. A second fault on an already-stalled
// job replaces the reason: it is the more recent thing the user has to act on.
//
// claimPause is true when Stall is about to pause a job whose intent was not
// already IntentPause, which makes the pause this application's.
func (app *Application) noteStall(jobID string, f *storagefault.Fault, claimPause bool) {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	app.setStallReasonLocked(jobID, "Stalled: "+f.Error(), claimPause)
}

// setStallReasonLocked records a reason, creating the record if needed.
//
// claimPause marks the record parked, and nothing here clears it: a record
// Stall already parked stays ours when a later reason arrives with
// claimPause false.
func (app *Application) setStallReasonLocked(jobID, reason string, claimPause bool) {
	rec, ok := app.stalls[jobID]
	if !ok {
		rec = &stallRecord{since: time.Now()}
		app.stalls[jobID] = rec
	}
	rec.reason = reason
	if claimPause {
		rec.parked = true
	}
}

// weParked reports whether this application paused the job, and so may resume
// it.
func (app *Application) weParked(jobID string) bool {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	rec, ok := app.stalls[jobID]
	return ok && rec.parked
}

// clearStall forgets a job's parked state, and reports whether the record it
// removed was one this application parked (weParked).
func (app *Application) clearStall(jobID string) (parked bool) {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	rec, ok := app.stalls[jobID]
	delete(app.stalls, jobID)
	return ok && rec.parked
}

// StallInfo is what a job's parked state looks like from outside the package.
type StallInfo struct {
	// Reason is the surfaced, actionable text R27 requires, or "" when the job
	// is not stalled.
	Reason string
	// Since is when the job first parked.
	Since time.Time
}

// StallReason reports why a job is parked, for the queue listing (R27).
//
// This in-memory map is the sole source, rather than a Header field: R19's
// re-evaluation needs a list of every parked job to walk, which a
// per-job Header field cannot provide, and clearStall/setStallReasonLocked
// are this map's only writers, giving the reason a single owner independent
// of anything Dispatcher does.
func (app *Application) StallReason(jobID string) StallInfo {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	rec, ok := app.stalls[jobID]
	if !ok || rec.reason == "" {
		return StallInfo{}
	}
	return StallInfo{Reason: rec.reason, Since: rec.since}
}

// ReevaluateStalls asks the stall loop to re-evaluate every parked job now,
// rather than at the next interval. This is R19's "and on user action".
//
// Non-blocking: it is called from the API's per-job resume handler and, under
// app.mu, from resumeLocked, neither of which may wait on a re-evaluation.
func (app *Application) ReevaluateStalls() {
	select {
	case app.stallKick <- struct{}{}:
	default:
		// A re-evaluation is already queued; a second one would do the same
		// work. Dropping it is not a lost request.
	}
}

// reevaluateStalls re-evaluates every parked job (R19).
func (app *Application) reevaluateStalls(ctx context.Context) {
	for _, jobID := range app.stalledJobIDs() {
		if ctx.Err() != nil {
			return
		}
		app.reevaluateStall(jobID)
	}
}

// reevaluateStall gets one parked job moving again: it resumes the job if this
// application paused it, and forgets the stall.
//
// Nothing is retried here. A write or completion fault left the affected
// articles Outstanding (a completion fault untrusts its file first), and a
// verification fault left the job non-resident, so a resumed job refetches
// and re-verifies through the ordinary paths. If the condition has not
// cleared, the next fault parks the job again.
func (app *Application) reevaluateStall(jobID string) {
	exists := false
	if app.dispatcher != nil {
		_, exists = app.dispatcher.Job(jobID)
	}
	if !exists {
		app.log.Info("stall re-evaluation: the job has left the queue; forgetting its parked state",
			"job", jobID)
		app.clearStall(jobID)
		return
	}
	// Only a job whose record says we paused it is resumed, so a user pause
	// that predates Stall is left alone. A user pause made after Stall's own
	// is not distinguished from it, and is undone here.
	if app.weParked(jobID) {
		switch err := app.dispatcher.ResumeJob(jobID); {
		case err == nil:
			app.log.Info("stall re-evaluated; the job has been resumed", "job", jobID)
		case errors.Is(err, dispatch.ErrUnwantedBlocked):
			// Blocked for an unwanted extension since: it stays paused for
			// the user, whose resume approves it.
			app.log.Info("stall re-evaluated; the job is blocked for unwanted extensions and stays paused",
				"job", jobID)
		default:
			app.log.Warn("stall re-evaluation: the job could not be resumed in dispatcher", "job", jobID, "err", err)
		}
	}
	app.clearStall(jobID)
	app.emit(Event{Type: "queue_updated", NzoID: jobID})
}
