pkg ./internal/app/
run ^(TestFinalize_KnownShortfallRetriesSized|TestFinalize_UnknownShortfallRetriesUnsized|TestFinalize_SizedRetryThatFallsShortRetriesOnceUnsized|TestFinalize_UnsizedRetryIsFinalEvenWithAHeldVolume|TestFinalize_RetryThatCannotStartLeavesNoLedgerEntry|TestFinalize_RetriedJobIsNotRetriedAgain|TestFinalize_RetriesOnlyAParErrorWithHeldVolumes|TestRetryWithHeldVolumes_NoEntry)$

# The finalizer's retry of a failed repair is sized by the shortfall par2
# reported, then unsized once, then final.

[the reported shortfall is ignored and the first retry is unsized]
file internal/app/job_finalizer.go
--- anchor
		return job.NeedBlocks(blocks), true
--- replace
		_ = blocks
		return job.Unsized(), true
--- end

[an unsized retry is retried again]
file internal/app/job_finalizer.go
--- anchor
	case wasRetried && prior == job.Unsized():
--- replace
	case false:
--- end

[a sized retry that falls short is retried sized again]
file internal/app/job_finalizer.go
--- anchor
	case wasRetried:
--- replace
	case false:
--- end

[a run with no held volume is retried]
file internal/app/job_finalizer.go
--- anchor
	case !heldVolumesMightRepair(ppJob):
--- replace
	case false:
--- end

[a retry's sizing is never looked up at the finalize of the retried instance]
file internal/app/job_finalizer.go
--- anchor
	prior, wasRetried := f.retries.take(ppJob.Job.ID())
--- replace
	var prior job.RecoverySizing
	wasRetried := false
	f.retries.take(ppJob.Job.ID())
--- end

[the sizing of a retry is not recorded]
file internal/app/job_finalizer.go
--- anchor
	f.retries.record(jobID, sizing)
--- replace
	_ = sizing
--- end

[the sizing of a retry that could not start is kept]
file internal/app/job_finalizer.go
--- anchor
		f.retries.forget(jobID)
--- replace
		_ = jobID
--- end

[the sizing is not taken out of the ledger when it is read]
file internal/app/job_finalizer.go
--- anchor
	s, ok := l.last[id]
	delete(l.last, id)
--- replace
	s, ok := l.last[id]
--- end

[the retry releases every volume whatever the sizing]
file internal/app/job_finalizer.go
--- anchor
			"post-processing failed par2 repair while the recovery volumes were held back", sizing)
--- replace
			"post-processing failed par2 repair while the recovery volumes were held back", job.Unsized())
--- end
