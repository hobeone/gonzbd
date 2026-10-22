pkg ./internal/assembler/
run Test(JobTombstone_DropsTheJobsFileTombstones|ForgetJob_DropsOnlyTheNamedJobsTombstones|CancelJob_RejectsLateArticles|CancelJob_KeepFilesStillTombstonesTheWholeJob)$

# The cancel and close-handles arms drop the per-file tombstones of the job
# they tombstone, so a finished job leaves one entry in the worker rather than
# one per file; the job-level tombstone alone keeps its late articles out.

[the cancel arm keeps the job's per-file tombstones]
file internal/assembler/assembler.go
--- anchor
		dropFileTombstones(completed, cancelID)
--- replace
		// mutated: per-file tombstones kept
--- end

[the close-handles arm keeps the job's per-file tombstones]
file internal/assembler/assembler.go
--- anchor
		dropFileTombstones(completed, targetID)
--- replace
		// mutated: per-file tombstones kept
--- end

[the sweep drops every job's per-file tombstones]
file internal/assembler/assembler.go
--- anchor
	maps.DeleteFunc(completed, func(k fileKey, _ struct{}) bool { return k.jobID == jobID })
--- replace
	maps.DeleteFunc(completed, func(fileKey, struct{}) bool { return true })
--- end

[the cancel arm sets no job-level tombstone]
file internal/assembler/assembler.go
--- anchor
		cancelledJobs[cancelID] = struct{}{}
--- replace
		_ = cancelID
--- end
