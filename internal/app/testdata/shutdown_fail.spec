pkg ./internal/app/
run TestFail_InTheCleanShutdownBarrier_DoesNotPersistAPartialJobForPostProcessing$

# A permanent storage fault routed to Application.Fail during the clean-shutdown
# barrier, then a restart over the same store.
#
# The first mutation is the hazard the test exists for, restated in the current
# state model: the post-processing hand-off advancing the job's position before
# the post-processor has it, so a shutdown that stops the post-processor first
# persists a partial download positioned for Assessing.
#
# The second is a stopping guard on Fail with nothing else changed. The job is
# then neither filed nor handed over while stopping, so the idle variant finds
# it still queued.

[the post-processing hand-off advances the job before the post-processor has it]
file internal/app/app.go
--- anchor
	du := app.duOrch.collect(j.ID())
--- replace
	_ = j.SetNext(job.Assessing)
	du := app.duOrch.collect(j.ID())
--- end

[Fail declines while the process is stopping]
file internal/app/durability.go
--- anchor
	reason := "Failed: " + f.Error()
--- replace
	if app.stopping.Load() {
		return
	}
	reason := "Failed: " + f.Error()
--- end
