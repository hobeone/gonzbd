pkg ./internal/queue/
run TestSetPostProcStarted

[the first-wins check dropped from the delegated setter]
file internal/queue/progress.go
--- anchor
	if !isJobStamp(t) || !p.downloadFinished.IsZero() {
--- replace
	if false {
--- end
