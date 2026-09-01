pkg ./internal/queue/
run TestDownloadStampWriters

[a fifth writer appears, by named assignment]
file internal/queue/job.go
--- anchor
func (j *Job) markStartedOnce(t time.Time) bool {
	return j.progress.setDownloadStartedOnce(t)
--- replace
func (j *Job) markStartedOnce(t time.Time) bool {
	j.progress.downloadStarted = t
	return j.progress.setDownloadStartedOnce(t)
--- end

[a fifth writer appears, via composite literal]
file internal/queue/job.go
--- anchor
func (j *Job) markDownloadFinishedOnce(t time.Time) bool {
	return j.progress.setDownloadFinishedOnce(t)
--- replace
func (j *Job) markDownloadFinishedOnce(t time.Time) bool {
	_ = JobProgress{downloadFinished: t}
	return j.progress.setDownloadFinishedOnce(t)
--- end

[a fifth writer appears, behind parentheses]
file internal/queue/job.go
--- anchor
func (j *Job) markStartedOnce(t time.Time) bool {
	return j.progress.setDownloadStartedOnce(t)
--- replace
func (j *Job) markStartedOnce(t time.Time) bool {
	(j.progress.downloadStarted) = t
	return j.progress.setDownloadStartedOnce(t)
--- end

[a fifth writer appears, through a pointer to the field]
file internal/queue/job.go
--- anchor
func (j *Job) markDownloadFinishedOnce(t time.Time) bool {
	return j.progress.setDownloadFinishedOnce(t)
--- replace
func (j *Job) markDownloadFinishedOnce(t time.Time) bool {
	ptr := &j.progress.downloadFinished
	*ptr = t
	return j.progress.setDownloadFinishedOnce(t)
--- end
