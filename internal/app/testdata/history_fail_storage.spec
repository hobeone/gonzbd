pkg ./internal/app/
run TestBuildHistoryEntry_FinalizeFailureUsesDownloadDir

[FailMsg alone does not mark history entry Failed]
file internal/app/history_helper.go
--- anchor
	if ppJob.ParError || ppJob.UnpackError || ppJob.FailMsg != "" {
--- replace
	if ppJob.ParError || ppJob.UnpackError {
--- end

[failed job leaves Storage at FinalDir]
file internal/app/history_helper.go
--- anchor
		entry.Storage = ppJob.DownloadDir
--- replace
		_ = ppJob.DownloadDir
--- end

[failed job leaves Path at FinalDir]
file internal/app/history_helper.go
--- anchor
		entry.Path = ppJob.DownloadDir
--- replace
		_ = ppJob.DownloadDir
--- end
