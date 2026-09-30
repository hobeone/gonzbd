pkg ./internal/dispatch/
run ^(TestLaunch_DeclinedLaunchReturnsTheGrant|TestLaunch_DeclinedLaunchLeavesALiveWorkersGrant|TestParkGrant_LogsARefusedPark|TestParkUnlaunched_ParksOnlyWithoutAClaim)$

# A grant with no worker is returned at two points: launch's first check, for
# a job whose intent is no longer IntentRun, and removeFor, for a job about to
# be deregistered. Each is neutered on its own, since they are reached by
# different events; the claim is what spares a live worker's grant.

[the first check declines a cancelled or paused job and keeps its grant]
file internal/dispatch/worker.go
--- anchor
	if v.Intent != job.IntentRun {
		d.parkUnlaunched(j)
		return
	}
--- replace
	if v.Intent != job.IntentRun {
		return
	}
--- end

[a live worker's grant is parked out from under it]
file internal/dispatch/worker.go
--- anchor
	_, claimed := d.launched[j.ID()]
--- replace
	_, claimed := d.launched[j.ID()]
	claimed = false
--- end

[a removal deregisters a job that still holds a grant]
file internal/dispatch/registry.go
--- anchor
	if j, ok := d.lookupFor(id, expected); ok {
		d.parkGrant(j)
	}
--- replace
	if j, ok := d.lookupFor(id, expected); ok && false {
		d.parkGrant(j)
	}
--- end

[a refused park is dropped silently]
file internal/dispatch/worker.go
--- anchor
	if err := d.q.Park(j); err != nil {
		d.log.Error("failed to return the resources of a job with no worker",
--- replace
	if err := d.q.Park(j); false && err != nil {
		d.log.Error("failed to return the resources of a job with no worker",
--- end
