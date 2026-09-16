pkg ./internal/app/
run TestFinalize_KeepsTheManifestWhenTheDispatcherRemoveFails

# The finalizer's copy of #376's ordering. Two mutations, because the flag
# guards two separate destructive steps and reverting one says nothing about
# the other.

[the manifest unlink no longer checks whether the queue row actually went]
file internal/app/job_finalizer.go
--- anchor
		if removedFromQueue && ppJob != nil && ppJob.Job != nil {
			_ = removeManifestIn(mdir, ppJob.Job.ID())
--- replace
		if ppJob != nil && ppJob.Job != nil {
			_ = removeManifestIn(mdir, ppJob.Job.ID())
--- end

[the durability delete no longer checks it either]
file internal/app/job_finalizer.go
--- anchor
		shouldDeleteDurability := removedFromQueue && entry.Status != string(constants.StatusFailed)
--- replace
		shouldDeleteDurability := entry.Status != string(constants.StatusFailed)
--- end
