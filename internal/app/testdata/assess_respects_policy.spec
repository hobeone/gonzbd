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
