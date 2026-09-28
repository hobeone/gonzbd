pkg ./internal/sched/
run ^(TestHandoff_AtFromRecordsNextParksAndCallsHanded|TestHandoff_StaleReportTouchesNothing|TestHandoff_ParksEvenWhenSetNextRefuses|TestHandoff_HandedRunsInsideTheQueueLock|TestHandoff_NilHandedIsAllowed)$

# Handoff's state check, its park on a refused verdict, and the one q.mu span
# its handed callback runs in. Each clause of the check is reverted on its own.

[a settled or never-run attempt is not refused]
file internal/sched/advance.go
--- anchor
	if !s.IsOpen() || s.State.State != from || s.State.Next != job.StateUnset {
--- replace
	if s.State.State != from || s.State.Next != job.StateUnset {
--- end

[a job that has left from is not refused]
file internal/sched/advance.go
--- anchor
	if !s.IsOpen() || s.State.State != from || s.State.Next != job.StateUnset {
--- replace
	if !s.IsOpen() || s.State.Next != job.StateUnset {
--- end

[a job whose next is recorded is not refused]
file internal/sched/advance.go
--- anchor
	if !s.IsOpen() || s.State.State != from || s.State.Next != job.StateUnset {
--- replace
	if !s.IsOpen() || s.State.State != from {
--- end

[a refused verdict returns before the park]
file internal/sched/advance.go
--- anchor
	err := j.SetNext(next)
	if perr := q.parkLocked(j); perr != nil {
--- replace
	err := j.SetNext(next)
	if err != nil {
		return true, err
	}
	if perr := q.parkLocked(j); perr != nil {
--- end

[handed runs after q.mu is released]
file internal/sched/advance.go
--- anchor
	if handed != nil {
		handed()
	}
	return true, err
--- replace
	q.mu.Unlock()
	if handed != nil {
		handed()
	}
	q.mu.Lock()
	return true, err
--- end

[handed is never called]
file internal/sched/advance.go
--- anchor
	if handed != nil {
		handed()
	}
	return true, err
--- replace
	return true, err
--- end
