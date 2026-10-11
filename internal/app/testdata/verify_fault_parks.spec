pkg ./internal/app/
run TestLooseRecord_LastingVerificationFaultStaysParked|TestReevaluateStall_TimerLeavesAVerifyFaultForTheOperator|TestReevaluateStall_TimerStillResumesAWriteFault|TestReevaluateStall_VerifyFaultOnAPausedJobKeepsItsReason|TestRunStallRecheck_ReevaluatesStallsOnItsTicker

[the timer lifts a verification fault as it does a write fault]
file internal/app/stall.go
--- anchor
	if by == byTimer && app.stallKindOf(jobID) == stallVerify {
--- replace
	if false {
--- end

[the timer lifts nothing, a write fault included]
file internal/app/stall.go
--- anchor
	if by == byTimer && app.stallKindOf(jobID) == stallVerify {
--- replace
	if by == byTimer {
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

[a kick re-evaluates as the timer does]
file internal/app/durability.go
--- anchor
			app.reevaluateStalls(ctx, byResume)
--- replace
			app.reevaluateStalls(ctx, byTimer)
--- end
