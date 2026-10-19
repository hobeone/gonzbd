pkg ./internal/postproc/
run ^TestQuickCheckStage_UnreadableSetBeside|^TestQuickCheckStage_LoneUnreadableSet|^TestRepairStage_UnreadableSetIsSkipped

# A par2 set that read as nothing keeps QuickCheck from a Clean or
# Unidentified verdict while the job holds a recovery volume, so repair runs
# and its failure fetches the volume (#1008). With none held, nothing can make
# the set readable: quickcheck records it in unverifiedPar2Sets and leaves it
# out of the verdict, and repair skips it by that record, so it costs the job
# only the verification of its own files.

[an unreadable set is ignored]
file internal/postproc/stage_quickcheck.go
--- anchor
	case len(unreadable) > 0:
--- replace
	case false:
--- end

[a deferred set outranks an unreadable one]
file internal/postproc/stage_quickcheck.go
--- anchor
	case len(unreadable) > 0:
--- replace
	case len(unreadable) > 0 && !(len(deferred) > 0 && len(deferred) == len(setsWithEntries(a.ID))):
--- end

[quickcheck leaves an unreadable set in the verdict with nothing held]
file internal/postproc/stage_quickcheck.go
--- anchor
	if len(unreadable) > 0 && !job.par2VolumesHeld() {
--- replace
	if false {
--- end

[quickcheck leaves an unreadable set out of the verdict with a volume held]
file internal/postproc/stage_quickcheck.go
--- anchor
	if len(unreadable) > 0 && !job.par2VolumesHeld() {
--- replace
	if len(unreadable) > 0 {
--- end

[quickcheck records nothing for repair to skip]
file internal/postproc/stage_quickcheck.go
--- anchor
		job.unverifiedPar2Sets = unreadable
--- replace
		job.unverifiedPar2Sets = nil
--- end

[a job of only unreadable sets reads as one with no CRC data]
file internal/postproc/stage_quickcheck.go
--- anchor
	case len(job.unverifiedPar2Sets) > 0 && len(setsWithEntries(a.ID)) == 0:
--- replace
	case false:
--- end

[repair runs a set quickcheck recorded unverified]
file internal/postproc/stage_repair.go
--- anchor
		if job.par2Unverified(set.Name) {
--- replace
		if false {
--- end

[repair skips every set]
file internal/postproc/stage_repair.go
--- anchor
		if job.par2Unverified(set.Name) {
--- replace
		if true {
--- end

[the owner reports a volume held with none]
file internal/postproc/stages.go
--- anchor
	return j.Job != nil && j.Job.HasDeferredPar2()
--- replace
	return j.Job != nil
--- end
