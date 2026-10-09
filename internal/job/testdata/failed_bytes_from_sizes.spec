pkg ./internal/job/
run TestRepairState_FailedBytesComeFromArticleSizes

[content failed bytes read as zero]
file internal/job/content.go
--- anchor
	return j.progress.ContentFailedBytes()
--- replace
	return 0
--- end
