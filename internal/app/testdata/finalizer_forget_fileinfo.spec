pkg ./internal/app/
run TestPersistAndCommit_DropsTheJobsFileInfo$

# Once the job has left the dispatcher nothing can re-register its files, so
# this forget is the one that bounds the pipeline's file-info cache.

[the finalizer leaves the job's file info]
file internal/app/job_finalizer.go
--- anchor
			app.pipeline.forgetJob(jobID)
--- replace
			_ = jobID
--- end
