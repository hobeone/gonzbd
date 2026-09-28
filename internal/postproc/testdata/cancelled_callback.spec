pkg ./internal/postproc/
run TestCancel_(QueuedJobFiresOnJobCancelled|InFlightJobFiresOnJobCancelledAfterStageReturns|UnknownIDFiresNothing)$|TestOnJobCancelled_NotFiredForACompletedJob$

# OnJobCancelled's two firing points, each removed on its own, and the
# in-flight one moved ahead of the stage it must wait for. Each is neutered by
# its condition or by where the call sits, so the code still compiles.

[a job Cancel takes out of the queue is not handed back]
file internal/postproc/postproc.go
--- anchor
	if removed && p.onJobCancelled != nil {
--- replace
	if false {
--- end

[a job cancelled mid-processing is not handed back]
file internal/postproc/postproc.go
--- anchor
			if p.onJobCancelled != nil {
				p.onJobCancelled(job)
			}
			continue
--- replace
			continue
--- end

[the handback runs before the stage has returned]
file internal/postproc/postproc.go
--- anchor
		p.processJob(jobCtx, job)
--- replace
		go func() {
			<-jobCtx.Done()
			if p.onJobCancelled != nil {
				p.onJobCancelled(job)
			}
		}()
		p.processJob(jobCtx, job)
--- end
