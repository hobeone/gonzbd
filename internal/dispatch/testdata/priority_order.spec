pkg ./internal/dispatch/
run TestTick_WalksInPriorityOrderWithPositionAsTheTieBreak|TestTick_HigherPriorityWinsTheOnlyLease|TestSetPriority_TakesEffectOnTheNextTick|TestRestore_KeepsPriorityOrder|TestList_FollowsDispatchOrder|TestTick_PositionTieBreakHoldsBeyondInsertionSortThreshold

[the tick walks d.order and ignores priority]
file internal/dispatch/registry.go
--- anchor
	ordered := d.dispatchOrderLocked()
	out := make([]*job.Job, 0, len(ordered))
--- replace
	ordered := make([]*entry, 0, len(d.order))
	for _, id := range d.order {
		ordered = append(ordered, d.byID[id])
	}
	out := make([]*job.Job, 0, len(ordered))
--- end

[List reads d.order and ignores priority]
file internal/dispatch/registry.go
--- anchor
	ordered := d.dispatchOrderLocked()
	ids := make([]string, 0, len(ordered))
--- replace
	ordered := make([]*entry, 0, len(d.order))
	for _, id := range d.order {
		ordered = append(ordered, d.byID[id])
	}
	ids := make([]string, 0, len(ordered))
--- end

[lower priority sorts first]
file internal/dispatch/registry.go
--- anchor
		return cmp.Or(cmp.Compare(b.e.h.Priority, a.e.h.Priority), cmp.Compare(a.pos, b.pos))
--- replace
		return cmp.Or(cmp.Compare(a.e.h.Priority, b.e.h.Priority), cmp.Compare(a.pos, b.pos))
--- end

[queue position is reversed within one priority]
file internal/dispatch/registry.go
--- anchor
		return cmp.Or(cmp.Compare(b.e.h.Priority, a.e.h.Priority), cmp.Compare(a.pos, b.pos))
--- replace
		return cmp.Or(cmp.Compare(b.e.h.Priority, a.e.h.Priority), cmp.Compare(b.pos, a.pos))
--- end

[SetPriority does not reach the header the order reads]
file internal/dispatch/registry.go
--- anchor
	e.h.Priority = priority
	d.kick()
--- replace
	_ = e
	d.kick()
--- end

[the position tie-break is dropped]
file internal/dispatch/registry.go
--- anchor
		return cmp.Or(cmp.Compare(b.e.h.Priority, a.e.h.Priority), cmp.Compare(a.pos, b.pos))
--- replace
		return cmp.Compare(b.e.h.Priority, a.e.h.Priority)
--- end
