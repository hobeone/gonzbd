pkg ./internal/sched/
run ^(TestHandoff_AtFromRecordsNextParksAndCallsHanded|TestHandoff_StaleReportTouchesNothing|TestHandoff_RefusedVerdictSettlesFailed|TestHandoff_NoVerdictParksWithoutRecordingNext|TestHandoff_HandedRunsInsideTheQueueLock|TestHandoff_NilHandedIsAllowed)$

# Handoff's state check, its Failed settle on a refused verdict, its park for a
# yield with no verdict, and the one q.mu span
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

[a refused verdict parks instead of settling Failed]
file internal/sched/advance.go
--- anchor
			if serr := q.settleLocked(j, job.OutcomeFailed, j.Snapshot()); serr != nil {
				err = errors.Join(err, serr)
			}
--- replace
			if serr := q.parkLocked(j); serr != nil {
				err = errors.Join(err, serr)
			}
--- end

[a yield with no verdict does not park]
file internal/sched/advance.go
--- anchor
	if err == nil {
		err = q.parkLocked(j)
	}
--- replace
	if err == nil && next != job.StateUnset {
		err = q.parkLocked(j)
	}
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
