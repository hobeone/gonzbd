pkg ./internal/app/
run TestHandleResult_DropsAResultFetchedForAnEarlierInstance|TestHandlers_ActOnTheResultsOwnInstance

# The pipeline's one gate on a result's instance, and the handlers behind it.
# The gate drops a result fetched for an instance the dispatcher no longer
# holds under its ID. Each handler mutation puts back a lookup of the ID where
# the handler acts on res.Job, which TestHandlers_ActOnTheResultsOwnInstance
# catches by calling the handlers with a result the gate would have dropped.

[the gate admits a result for any instance under the registered ID]
file internal/app/pipeline.go
--- anchor
	return ok && cur == res.Job
--- replace
	return ok && cur != nil
--- end

[a terminal failure registers the file of whatever instance holds the ID]
file internal/app/pipeline.go
--- anchor
		if err := p.registerFile(res.Job, res.FileIdx); err != nil {
			p.log.Warn("register fallback file failed",
--- replace
		if err := p.registerFile(func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }(), res.FileIdx); err != nil {
			p.log.Warn("register fallback file failed",
--- end

[a terminal failure is recorded on whatever instance holds the ID]
file internal/app/pipeline.go
--- anchor
		if err := res.Job.MarkArticleFailed(int(res.ArtIdx)); err != nil {
--- replace
		if err := func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }().MarkArticleFailed(int(res.ArtIdx)); err != nil {
--- end

[a failed fatal write returns whatever instance holds the ID to the pool]
file internal/app/pipeline.go
--- anchor
"job", res.JobID(), "msgid", res.MessageID, "err", writeErr)
			_ = res.Job.ClearArticleEmitted(int(res.ArtIdx))
		}
		telemetry.ArticlesFailed.Add(1)
--- replace
"job", res.JobID(), "msgid", res.MessageID, "err", writeErr)
			_ = func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }().ClearArticleEmitted(int(res.ArtIdx))
		}
		telemetry.ArticlesFailed.Add(1)
--- end

[early abort is judged on whatever instance holds the ID]
file internal/app/pipeline.go
--- anchor
		if res.Job.CheckEarlyAbort() {
--- replace
		if func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }().CheckEarlyAbort() {
--- end

[a retryable failure returns whatever instance holds the ID to the pool]
file internal/app/pipeline.go
--- anchor
"err", res.Err)
		_ = res.Job.ClearArticleEmitted(int(res.ArtIdx))
		telemetry.ArticlesRetried.Add(1)
--- replace
"err", res.Err)
		_ = func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }().ClearArticleEmitted(int(res.ArtIdx))
		telemetry.ArticlesRetried.Add(1)
--- end

[a success starts whatever instance holds the ID]
file internal/app/pipeline.go
--- anchor
	_ = res.Job.MarkJobStarted(time.Now())
--- replace
	_ = func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }().MarkJobStarted(time.Now())
--- end

[a success credits whatever instance holds the ID]
file internal/app/pipeline.go
--- anchor
	_ = res.Job.RecordDownload(res.ServerName, len(res.Data))
--- replace
	_ = func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }().RecordDownload(res.ServerName, len(res.Data))
--- end

[a success registers the file of whatever instance holds the ID]
file internal/app/pipeline.go
--- anchor
	if err := p.registerFile(res.Job, res.FileIdx); err != nil {
		p.log.Warn("register file failed",
--- replace
	if err := p.registerFile(func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }(), res.FileIdx); err != nil {
		p.log.Warn("register file failed",
--- end

[a failed registration returns whatever instance holds the ID to the pool]
file internal/app/pipeline.go
--- anchor
"fileidx", res.FileIdx, "err", err)
		_ = res.Job.ClearArticleEmitted(int(res.ArtIdx))
		return
--- replace
"fileidx", res.FileIdx, "err", err)
		_ = func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }().ClearArticleEmitted(int(res.ArtIdx))
		return
--- end

[a failed write returns whatever instance holds the ID to the pool]
file internal/app/pipeline.go
--- anchor
"err", writeErr)
		_ = res.Job.ClearArticleEmitted(int(res.ArtIdx))
	} else if writeErr == nil {
--- replace
"err", writeErr)
		_ = func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }().ClearArticleEmitted(int(res.ArtIdx))
	} else if writeErr == nil {
--- end
