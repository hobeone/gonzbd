pkg ./internal/dispatch/
run ^(TestAdvanceFrom_LateReportLeavesTheNextStatesWorkerAlone|TestAdvanceFrom_UnlaunchedReportLaunchesTheNextStateOnce|TestAdvanceFrom_RecordedNextIsStale|TestAdvanceFrom_RefusedVerdictSettlesFailed|TestAdvanceFrom_NoVerdictIsRefused|TestAdvanceFrom_OtherInstanceIsNotFound|TestYieldedFrom_ReleasesOnlyTheNamedState|TestClearLaunchedFor_LeavesALaterInstancesClaim|TestDispatcherHandoff_NamesItsDoorAndRefusesStaleReports)$

# AdvanceFrom and YieldedFrom, the state-scoped exit reports. The first
# mutation is the double launch: a report for a state the job has left parks
# the next state's worker and clears its claim.

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
		return fmt.Errorf("dispatch: %s: no job %q: %w", door, id, ErrNotFound)
--- replace
	if false {
		return fmt.Errorf("dispatch: %s: no job %q: %w", door, id, ErrNotFound)
--- end

[AdvanceFrom accepts StateUnset as a verdict]
file internal/dispatch/worker.go
--- anchor
	if next == job.StateUnset {
		return fmt.Errorf("dispatch: AdvanceFrom: %w: StateUnset is not a verdict", job.ErrIllegalTransition)
	}
--- replace
--- end

[YieldedFrom ignores the state it names]
file internal/dispatch/worker.go
--- anchor
	return d.handoff("YieldedFrom", j, from, job.StateUnset)
--- replace
	return d.YieldedJob(j)
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
