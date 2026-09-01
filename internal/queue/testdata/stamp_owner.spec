pkg ./internal/queue/
run TestIsJobStamp|TestSetDownloadStamp|TestClearDownloadStamps|TestRestoreDownloadStamps

[the bound relaxed to After(epoch), admitting the sub-second interval]
file internal/queue/progress.go
--- anchor
func isJobStamp(t time.Time) bool { return t.Unix() > 0 }
--- replace
func isJobStamp(t time.Time) bool { return t.After(time.Unix(0, 0)) }
--- end

[the bound relaxed to the IsZero test it replaces]
file internal/queue/progress.go
--- anchor
func isJobStamp(t time.Time) bool { return t.Unix() > 0 }
--- replace
func isJobStamp(t time.Time) bool { return !t.IsZero() }
--- end

[first-wins neutered on started]
file internal/queue/progress.go
--- anchor
	if !isJobStamp(t) || !p.downloadStarted.IsZero() {
--- replace
	if !isJobStamp(t) {
--- end

[first-wins neutered on finished]
file internal/queue/progress.go
--- anchor
	if !isJobStamp(t) || !p.downloadFinished.IsZero() {
--- replace
	if !isJobStamp(t) {
--- end

[clear forgets the finished field]
file internal/queue/progress.go
--- anchor
func (p *JobProgress) clearDownloadStamps() {
	p.downloadStarted = time.Time{}
	p.downloadFinished = time.Time{}
--- replace
func (p *JobProgress) clearDownloadStamps() {
	p.downloadStarted = time.Time{}
--- end

[restore stops filtering the started field]
file internal/queue/progress.go
--- anchor
	if isJobStamp(started) {
		p.downloadStarted = started
	}
--- replace
	p.downloadStarted = started
--- end

[restore stops filtering the finished field]
file internal/queue/progress.go
--- anchor
	if isJobStamp(finished) {
		p.downloadFinished = finished
	}
--- replace
	p.downloadFinished = finished
--- end
