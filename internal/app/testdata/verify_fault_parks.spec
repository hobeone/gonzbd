pkg ./internal/app/
run TestLooseRecord_LastingVerificationFaultStaysParked|TestReevaluateStall_LeavesAVerifyFaultUntilTheJobIsResumed|TestReevaluateStall_TimerStillResumesAWriteFault|TestReevaluateStall_VerifyFaultOnAPausedJobKeepsItsReason|TestRunStallRecheck_ReevaluatesStallsOnItsTicker

[a re-evaluation lifts a verification fault as it does a write fault]
file internal/app/stall.go
--- anchor
	if app.stallKindOf(jobID) == stallVerify && j.Intent() == job.IntentPause {
--- replace
	if _ = j; false {
--- end

[a verification fault is never lifted, even once the job is resumed]
file internal/app/stall.go
--- anchor
	if app.stallKindOf(jobID) == stallVerify && j.Intent() == job.IntentPause {
--- replace
	if _ = j; app.stallKindOf(jobID) == stallVerify {
--- end

[a write fault on a paused job is kept as a verification fault is]
file internal/app/stall.go
--- anchor
	if app.stallKindOf(jobID) == stallVerify && j.Intent() == job.IntentPause {
--- replace
	if j.Intent() == job.IntentPause {
--- end

[the residency parks through Stall, as a retryable fault]
file internal/app/app.go
--- anchor
	app.residency.parked = app.parkVerifyFault
--- replace
	app.residency.parked = app.Stall
--- end

[the record drops the fault's kind]
file internal/app/stall.go
--- anchor
	app.stalls[jobID].kind = kind
--- replace
	app.stalls[jobID].kind = stallRetryable
--- end
