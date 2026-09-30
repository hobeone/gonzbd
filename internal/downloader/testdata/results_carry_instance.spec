pkg ./internal/downloader/
run TestDownloader_MarksOnlyTheInstanceAFetchWasDispatchedFor|TestDownloader_ClearsOnlyTheInstanceAFetchWasDispatchedFor|TestFetchArticle_DropsARequestForARemovedInstance

# Each site where the downloader mutates a job for a request acts on the
# instance the request was dispatched for. Every mutation below puts back the
# by-ID lookup that finds a retry registered under the same ID instead.

[an Emitted mark lands on whatever instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
func (d *Downloader) markEmitted(req *articleRequest) {
--- replace
func (d *Downloader) markEmitted(req *articleRequest) {
	if cur, ok := d.dispatcher.Job(req.jobID()); ok {
		req = &articleRequest{job: cur, artIdx: req.artIdx, messageID: req.messageID}
	}
--- end

[a dropped result clears whatever instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
		if err := req.job.ClearArticleEmitted(int(req.artIdx)); err != nil {
--- replace
		if cur, ok := d.dispatcher.Job(req.jobID()); ok {
			req = &articleRequest{job: cur, artIdx: req.artIdx, messageID: req.messageID}
		}
		if err := req.job.ClearArticleEmitted(int(req.artIdx)); err != nil {
--- end

[a request drained under a global pause clears whatever instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
	if d.paused.Load() || d.dispatcher.Paused() {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
--- replace
	if d.paused.Load() || d.dispatcher.Paused() {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		if cur, ok := d.dispatcher.Job(req.jobID()); ok {
			_ = cur.ClearArticleEmitted(int(req.artIdx))
		}
--- end

[the pre-fetch intent check reads whatever instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
	if _, ok := d.dispatcher.Job(req.jobID()); !ok || req.job.Intent() != job.IntentRun {
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur.Intent() != job.IntentRun {
--- end

[a request dropped by the intent check clears whatever instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
	if _, ok := d.dispatcher.Job(req.jobID()); !ok || req.job.Intent() != job.IntentRun {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
--- replace
	if _, ok := d.dispatcher.Job(req.jobID()); !ok || req.job.Intent() != job.IntentRun {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		if cur, ok := d.dispatcher.Job(req.jobID()); ok {
			_ = cur.ClearArticleEmitted(int(req.artIdx))
		}
--- end
