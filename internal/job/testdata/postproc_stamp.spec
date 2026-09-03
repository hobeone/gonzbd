pkg ./internal/job/
run TestMarkDownloadFinished_FirstWins

[the first-wins check dropped from the delegated setter]
file internal/job/progress.go
--- anchor
	if !isJobStamp(t) || !p.downloadFinished.IsZero() {
--- replace
	if false {
--- end
