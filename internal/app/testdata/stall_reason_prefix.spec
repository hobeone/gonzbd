pkg ./internal/app/
run ^TestStallReason_SaysWhetherAResumeIsNeeded$

[a verification fault carries the plain prefix]
file internal/app/stall.go
--- anchor
	if k == stallVerify {
--- replace
	if false {
--- end

[a retryable fault carries the resume prefix]
file internal/app/stall.go
--- anchor
	if k == stallVerify {
--- replace
	if true {
--- end

[the reason ignores the fault's kind]
file internal/app/stall.go
--- anchor
	app.setStallReasonLocked(jobID, kind.reasonPrefix()+f.Error(), claimPause)
--- replace
	app.setStallReasonLocked(jobID, stallRetryable.reasonPrefix()+f.Error(), claimPause)
--- end
