pkg ./internal/queue/
run TestStampFieldOwners

[an alias to JobProgress appears]
file internal/queue/progress.go
--- anchor
func (p *JobProgress) setDownloadStartedOnce(t time.Time) bool {
--- replace
type jobProgressAlias = JobProgress

func (p *JobProgress) setDownloadStartedOnce(t time.Time) bool {
--- end
