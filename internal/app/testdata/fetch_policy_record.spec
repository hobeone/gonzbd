pkg ./internal/app/
run TestPermanentArticleFailure_LeavesTheVolumeHeldUntilReleased|TestMaybeReleaseRecoveryVolumes_MarksThePolicyForCheckpointing

# A fetch policy reaches job_files because its one writer records the change
# and the recorder's flush takes it (#858). The two halves are neutered
# separately: one being pinned says nothing about the other.

[the policy's writer records no change]
file internal/job/content.go
--- anchor
	if !f.fetchChanged {
		f.fetchChanged = true
--- replace
	if false {
		f.fetchChanged = true
--- end

[the flush does not take the recorded changes]
file internal/app/record.go
--- anchor
	r.takeFetchPolicyChanges()
	live := r.liveInstances()
--- replace
	live := r.liveInstances()
--- end
