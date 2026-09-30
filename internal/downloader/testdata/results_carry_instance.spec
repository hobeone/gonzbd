pkg ./internal/downloader/
run TestDownloader_MarksOnlyTheInstanceAFetchWasDispatchedFor|TestEmitResult_ClearsOnlyTheInstanceADroppedResultWasFor|TestFetchArticle_DropsARequestForAnInstanceNoLongerRegistered|TestFetchArticle_GlobalPauseClearsTheRequestsOwnMark

# Each site where the downloader mutates a job for a request acts on the
# instance the request was dispatched for. The "whatever instance holds the
# ID" mutations put back the by-ID lookup that finds a retry registered under
# the same ID instead; the "neutered" ones leave the call in place but make it
# fail, so the request's own instance is not updated.
#
# The global-pause clear has no by-ID mutation: a request reaches it only when
# its instance is the registered one, so a lookup there finds the same
# instance and the two are indistinguishable.

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

[markEmitted marks nothing]
file internal/downloader/dispatch.go
--- anchor
	if err := req.job.MarkArticleEmitted(int(req.artIdx)); err != nil {
--- replace
	if err := req.job.MarkArticleEmitted(-1); err != nil {
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

[a dropped result clears nothing]
file internal/downloader/dispatch.go
--- anchor
		if err := req.job.ClearArticleEmitted(int(req.artIdx)); err != nil {
--- replace
		if err := req.job.ClearArticleEmitted(-1); err != nil {
--- end

[a request drained under a global pause clears nothing]
file internal/downloader/dispatch.go
--- anchor
	if d.paused.Load() || d.dispatcher.Paused() {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
--- replace
	if d.paused.Load() || d.dispatcher.Paused() {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(-1)
--- end

[the pre-fetch check ignores which instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun {
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur == nil || req.job.Intent() != job.IntentRun {
--- end

[a request dropped by the pre-fetch check clears whatever instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		if ok {
			_ = cur.ClearArticleEmitted(int(req.artIdx))
		}
--- end

[a request dropped by the pre-fetch check clears nothing]
file internal/downloader/dispatch.go
--- anchor
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(-1)
--- end
