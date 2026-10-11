pkg ./internal/app/
run TestHandleArticlesUnwritten_ReturnsTheArticlesToOutstanding|TestHandleArticlesUnwritten_ClearsTheBitsOfAnEvictedJob|TestPipeline_HandleFailureResult|TestPipeline_HandleSuccessResult|TestHandleResult_DropsAResultFetchedForAnEarlierInstance|TestHandlers_ActOnTheResultsOwnInstance|TestHandleSuccessResult_ReturnsTheArticleWhenTheFileCannotBeRegistered

[an evicted job's emitted clear neutered]
file internal/job/content.go
--- anchor
		j.progress.emitted.Clear(artIdx)
--- replace
		_ = artIdx
--- end

[a resident job's emitted clear neutered]
file internal/job/content.go
--- anchor
	j.progress.clearEmitted(j.manifest, artIdx)
--- replace
	_ = artIdx
--- end

[the emitted mark never set]
file internal/job/progress.go
--- anchor
	p.emitted.Set(i)
--- replace
	_ = i
--- end
