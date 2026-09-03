pkg ./internal/downloader/
run TestEmitResult_ReportsAFailureToClearRatherThanSwallowingIt|TestDispatchPass_QueuePausedSkipsDispatch

[bounds check on ClearArticleEmitted neutered]
file internal/job/content.go
--- anchor
func (j *Job) ClearArticleEmitted(artIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil || j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if artIdx < 0 || artIdx >= j.manifest.NumArticles() {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
--- replace
func (j *Job) ClearArticleEmitted(artIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil || j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if false {
		return nil
	}
--- end

[dispatcher.Paused check in dispatchPass neutered]
file internal/downloader/dispatch.go
--- anchor
func (d *Downloader) dispatchPass(ctx context.Context) {
	if d.paused.Load() || d.dispatcher.Paused() {
		return
	}
--- replace
func (d *Downloader) dispatchPass(ctx context.Context) {
	if d.paused.Load() || false {
		return
	}
--- end
