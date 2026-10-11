pkg ./internal/downloader/
run TestFetchArticle_GlobalPauseClearsTheRequestsOwnMark|TestEmitResult_ClearsOnlyTheInstanceADroppedResultWasFor|TestFetchArticle_DropsARequestForAnInstanceNoLongerRegistered|TestDownloader_MarksOnlyTheInstanceAFetchWasDispatchedFor

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
