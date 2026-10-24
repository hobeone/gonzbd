pkg ./internal/app/
run TestWriteArticle_AfterTheJobClosedCreatesNoFile$

# #1053's acceptance: a write that reaches the assembler worker after the job
# left the dispatcher or was handed off creates no file. The resolver's two
# checks are what stand in its way, on a cache hit and on the miss that
# follows a RemoveJob's or enqueuePostProc's forgetJob.

[a miss for a job the dispatcher no longer holds is a plain miss]
file internal/app/pipeline.go
--- anchor
		cur, ok := p.dispatcher.Job(jobID)
		if !ok {
--- replace
		cur, _ := p.dispatcher.Job(jobID)
		if false {
--- end

[a miss is judged without the held instance]
file internal/app/pipeline.go
--- anchor
		j = cur
--- replace
		_ = cur
--- end

[the resolver ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	} else if !p.holds(j) {
--- replace
	} else if false {
--- end

[the resolver ignores the hand-over]
file internal/app/pipeline.go
--- anchor
	if p.handedOff != nil && p.handedOff(j) {
--- replace
	if false {
--- end
