pkg ./internal/app/
run TestPipeline_GivesBackTheBudgetOnEveryPath

[a result for an instance no longer registered keeps its hold]
file internal/app/pipeline.go
--- anchor
			"job", res.JobID(), "msgid", res.MessageID, "err", res.Err)
		res.Release()
--- replace
			"job", res.JobID(), "msgid", res.MessageID, "err", res.Err)
		_ = res
--- end

[a failed result keeps its hold]
file internal/app/pipeline.go
--- anchor
func (p *pipeline) handleFailureResult(ctx context.Context, res *downloader.ArticleResult) {
	res.Release()
--- replace
func (p *pipeline) handleFailureResult(ctx context.Context, res *downloader.ArticleResult) {
	_ = res
--- end

[a success result the assembler never took keeps its hold]
file internal/app/pipeline.go
--- anchor
		if !bufferConsumed {
			res.Release()
--- replace
		if !bufferConsumed {
			_ = res
--- end

[a written article's hold is not handed to the assembler]
file internal/app/pipeline.go
--- anchor
		Release: res.ReleaseHeld,
--- replace
		Release: nil,
--- end
