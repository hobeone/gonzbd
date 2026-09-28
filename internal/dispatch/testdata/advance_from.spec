pkg ./internal/dispatch/
run ^(TestAdvanceFrom_LateReportLeavesTheNextStatesWorkerAlone|TestAdvanceFrom_UnlaunchedReportLaunchesTheNextStateOnce|TestAdvanceFrom_RecordedNextIsStale|TestAdvanceFrom_RefusedVerdictStillReleasesTheWorker|TestAdvanceFrom_OtherInstanceIsNotFound|TestClearLaunchedFor_LeavesALaterInstancesClaim)$

# AdvanceFrom, the one exit report for finished work. The first mutation is
# the double launch: a report for a state the job has left parks the next
# state's worker and clears its claim.

[a report for a state the job has left still acts]
file internal/sched/advance.go
--- anchor
	if !s.IsOpen() || s.State.State != from || s.State.Next != job.StateUnset {
--- replace
	if !s.IsOpen() || s.State.Next != job.StateUnset {
--- end

[a report for a job whose next is recorded still acts]
file internal/sched/advance.go
--- anchor
	if !s.IsOpen() || s.State.State != from || s.State.Next != job.StateUnset {
--- replace
	if !s.IsOpen() || s.State.State != from {
--- end

[the report does not release its worker's claim]
file internal/dispatch/worker.go
--- anchor
	handed, err := d.q.Handoff(j, from, next, func() { d.clearLaunchedFor(j) })
--- replace
	handed, err := d.q.Handoff(j, from, next, nil)
--- end

[the report acts on an instance that is not registered]
file internal/dispatch/worker.go
--- anchor
	if _, ok := d.lookupFor(id, j); !ok {
		return fmt.Errorf("dispatch: AdvanceFrom: no job %q: %w", id, ErrNotFound)
--- replace
	if false {
		return fmt.Errorf("dispatch: AdvanceFrom: no job %q: %w", id, ErrNotFound)
--- end

[clearLaunchedFor clears whatever claim the ID carries]
file internal/dispatch/worker.go
--- anchor
	if _, ok := d.lookupFor(j.ID(), j); ok {
		d.clearLaunched(j.ID())
--- replace
	if true {
		d.clearLaunched(j.ID())
--- end
