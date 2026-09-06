pkg ./internal/job/
run TestResetForRetry_ClearsDownloadStamps

[ResetForRetry stops clearing the download stamps]
file internal/job/content.go
--- anchor
	j.progress.clearDownloadStamps()
--- replace
	_ = j.progress
--- end
