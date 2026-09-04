pkg ./internal/job/
run TestDownloadStampWriters

[a fourth writer appears, by named assignment]
file internal/job/progress.go
--- anchor
func (p *JobProgress) Par2Recovered() bool {
	if p == nil {
		return false
	}
--- replace
func (p *JobProgress) Par2Recovered() bool {
	p.downloadStarted = time.Time{}
	if p == nil {
		return false
	}
--- end

[a fourth writer appears, via composite literal]
file internal/job/progress.go
--- anchor
func (p *JobProgress) Par2Recovered() bool {
	if p == nil {
		return false
	}
--- replace
func (p *JobProgress) Par2Recovered() bool {
	_ = JobProgress{downloadFinished: time.Time{}}
	if p == nil {
		return false
	}
--- end

[a fourth writer appears, behind parentheses]
file internal/job/progress.go
--- anchor
func (p *JobProgress) Par2Recovered() bool {
	if p == nil {
		return false
	}
--- replace
func (p *JobProgress) Par2Recovered() bool {
	(p.downloadStarted) = time.Time{}
	if p == nil {
		return false
	}
--- end

[a fourth writer appears, through a pointer to the field]
file internal/job/progress.go
--- anchor
func (p *JobProgress) Par2Recovered() bool {
	if p == nil {
		return false
	}
--- replace
func (p *JobProgress) Par2Recovered() bool {
	ptr := &p.downloadFinished
	*ptr = time.Time{}
	if p == nil {
		return false
	}
--- end
