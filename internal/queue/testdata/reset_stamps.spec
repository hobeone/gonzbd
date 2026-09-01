pkg ./internal/queue/
run TestResetForRetry_OnlyTouchesFailedArticles

[ResetForRetry stops clearing the download stamps]
file internal/queue/job.go
--- anchor
	j.progress.clearDownloadStamps()
--- replace
	_ = j.progress
--- end
