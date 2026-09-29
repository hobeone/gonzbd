pkg ./internal/dispatch/
run ^(TestLaunch_DeclinedLaunchReturnsTheGrant|TestLaunch_DeclinedLaunchLeavesALiveWorkersGrant|TestParkGrant_LogsARefusedPark)$

# Every path on which launch declines a job that holds what Advance granted it
# gives that grant back, and the claim is what spares a live worker's grant.
# Each park is neutered on its own: the three paths are reached by different
# windows, so one being pinned says nothing about the others.

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

[a refused claim keeps the grant of a job being removed]
file internal/dispatch/worker.go
--- anchor
	if !d.claimLaunched(j.ID()) {
		d.parkUnlaunched(j)
		return
	}
--- replace
	if !d.claimLaunched(j.ID()) {
		return
	}
--- end

[the failed re-check clears its claim and keeps the grant]
file internal/dispatch/worker.go
--- anchor
		if v.Running {
			d.parkGrant(j)
		}
--- replace
		if false {
			d.parkGrant(j)
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

[a refused park is dropped silently]
file internal/dispatch/worker.go
--- anchor
	if err := d.q.Park(j); err != nil {
		d.log.Error("failed to return the resources of a job that was not launched",
--- replace
	if err := d.q.Park(j); false && err != nil {
		d.log.Error("failed to return the resources of a job that was not launched",
--- end
