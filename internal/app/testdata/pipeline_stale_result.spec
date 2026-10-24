pkg ./internal/app/
run TestHandleResult_DropsAResultFetchedForAnEarlierInstance|TestHandlers_ActOnTheResultsOwnInstance

# The pipeline's one gate on a result's instance, and the handlers behind it.
# The gate drops a result fetched for an instance the dispatcher no longer
# holds under its ID. Each handler mutation puts back a lookup of the ID where
# the handler acts on res.Job, which TestHandlers_ActOnTheResultsOwnInstance
# catches by calling the handlers with a result the gate would have dropped.
#
# Only the sites BEFORE registerFile are mutated. registerFile refuses an
# instance the dispatcher does not hold (pipeline_closed.spec), so past it a
# stale result is gone and res.Job is the dispatcher's instance by
# construction: a by-ID lookup at MarkArticleFailed, CheckEarlyAbort or the
# post-write clearEmitted is observationally equivalent, and no fixture can
# make it differ without a hook in the window between the guard and the site.

[the gate admits a result for any instance under the registered ID]
file internal/app/pipeline.go
--- anchor
	return ok && cur == j
--- replace
	return ok && cur != nil
--- end

[a handler registers the file of whatever instance holds the ID]
file internal/app/pipeline.go
--- anchor
	err := p.registerFile(res.Job, res.FileIdx)
--- replace
	err := p.registerFile(func() *job.Job { j, _ := p.dispatcher.Job(res.JobID()); return j }(), res.FileIdx)
--- end

[a retryable failure returns whatever instance holds the ID to the pool]
file internal/app/pipeline.go
--- anchor
"err", res.Err)
		p.clearEmitted(res)
		telemetry.ArticlesRetried.Add(1)
--- replace
"err", res.Err)
		p.clearEmitted(func() *downloader.ArticleResult { c := *res; c.Job, _ = p.dispatcher.Job(res.JobID()); return &c }())
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

[a failed registration returns whatever instance holds the ID to the pool]
file internal/app/pipeline.go
--- anchor
	p.clearEmitted(res)
	return false
--- replace
	p.clearEmitted(func() *downloader.ArticleResult { c := *res; c.Job, _ = p.dispatcher.Job(res.JobID()); return &c }())
	return false
--- end
