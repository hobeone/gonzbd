pkg ./internal/job/
run TestDownloadStampWriters

[a fourth writer appears, by named assignment]
file internal/job/progress.go
--- anchor
func (p *JobProgress) Par2ReleaseReason() string {
	if p == nil {
		return ""
	}
--- replace
func (p *JobProgress) Par2ReleaseReason() string {
	p.downloadStarted = time.Time{}
	if p == nil {
		return ""
	}
--- end

[a fourth writer appears, via composite literal]
file internal/job/progress.go
--- anchor
func (p *JobProgress) Par2ReleaseReason() string {
	if p == nil {
		return ""
	}
--- replace
func (p *JobProgress) Par2ReleaseReason() string {
	_ = JobProgress{downloadFinished: time.Time{}}
	if p == nil {
		return ""
	}
--- end

[a fourth writer appears, behind parentheses]
file internal/job/progress.go
--- anchor
func (p *JobProgress) Par2ReleaseReason() string {
	if p == nil {
		return ""
	}
--- replace
func (p *JobProgress) Par2ReleaseReason() string {
	(p.downloadStarted) = time.Time{}
	if p == nil {
		return ""
	}
--- end

[a fourth writer appears, through a pointer to the field]
file internal/job/progress.go
--- anchor
func (p *JobProgress) Par2ReleaseReason() string {
	if p == nil {
		return ""
	}
--- replace
func (p *JobProgress) Par2ReleaseReason() string {
	ptr := &p.downloadFinished
	*ptr = time.Time{}
	if p == nil {
		return ""
	}
--- end
