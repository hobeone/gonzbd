pkg ./internal/app/
run TestMaybeReleaseRecoveryVolumes_RespectsRepairPolicy$

# maybeReleaseRecoveryVolumes's outcomeRepair branch un-deferring recovery
# volumes for a job whose Policy.Repair is false (#653: PP=0 never enters
# Repairing, so fetching them only spends bandwidth). Neutering the guard
# makes the branch release unconditionally, as it did before the fix.

[the outcomeRepair branch always releases, ignoring Policy.Repair]
file internal/app/app.go
--- anchor
		if !j.Policy().Repair {
--- replace
		if false {
--- end

# PR #686 review: a verdict WAS reached for a !Policy.Repair job even though
# it is not acted on, so the release reason must still be recorded. Dropping
# the call left the job looking like a verdict was never reached.

[PP=0 outcomeRepair drops SetPar2ReleaseReason]
file internal/app/app.go
--- anchor
			j.SetPar2ReleaseReason(reason)
			app.log.Info("on-demand par2: repair needed but the job's policy forbids repair; holding the volumes and finalizing",
--- replace
			app.log.Info("on-demand par2: repair needed but the job's policy forbids repair; holding the volumes and finalizing",
--- end
