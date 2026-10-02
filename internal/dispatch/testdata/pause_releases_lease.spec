pkg ./internal/dispatch/
run ^(TestPauseJob_FetchingJobFreesItsLease|TestPauseJob_RefusesACancelledJobAndYieldsNothing|TestPauseJob_KeepsThePausedJobResident|TestResumeJob_ContinuesWithoutRehydrating|TestRestore_PausedJobIsNotHydratedUntilResumed|TestRemove_PausedResidentJobIsEvicted|TestPause_QueueWidePauseKeepsLeases)$

# A per-job pause returns a Fetching job's lease (PauseJob's yield), keeps a
# resident paused job's manifest (reconcileResidency's eviction arm) and never
# hydrates one (its hydrate arm); a removal still evicts a paused resident
# job, and a queue-wide pause still leaves a working job its lease. A pause the
# cancel latch refuses yields nothing.

[a refused pause still yields]
file internal/dispatch/registry.go
--- anchor
	if err := j.SetIntent(job.IntentPause); err != nil {
		return fmt.Errorf("dispatch: pause %s: %w", id, err)
	}
--- replace
	if err := j.SetIntent(job.IntentPause); err != nil {
		_ = d.YieldedFrom(j, job.Fetching)
		return fmt.Errorf("dispatch: pause %s: %w", id, err)
	}
--- end

[a per-job pause keeps the Fetching job's lease]
file internal/dispatch/registry.go
--- anchor
	if err := d.YieldedFrom(j, job.Fetching); err != nil && !errors.Is(err, ErrStaleReport) {
--- replace
	if err := error(nil); err != nil && !errors.Is(err, ErrStaleReport) {
--- end

[a paused job that gave back its lease is evicted]
file internal/dispatch/tick.go
--- anchor
	case !v.Holds && v.Intent != job.IntentPause && d.isResident(j.ID()):
--- replace
	case !v.Holds && d.isResident(j.ID()):
--- end

[a paused job is hydrated because it is paused]
file internal/dispatch/tick.go
--- anchor
	case v.Holds && !d.isResident(j.ID()):
--- replace
	case (v.Holds || v.Intent == job.IntentPause) && !d.isResident(j.ID()):
--- end

[a removal leaves the paused job's manifest loaded]
file internal/dispatch/registry.go
--- anchor
	d.res.Evict(id)
--- replace
	_ = id
--- end

[a queue-wide pause strips a working job's lease]
file internal/sched/advance.go
--- anchor
		if q.holds(j.ID(), s) {
--- replace
		if q.holds(j.ID(), s) && !q.paused {
--- end
