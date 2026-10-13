pkg ./internal/app/
run TestBuildHistoryEntry_FinalizeFailureUsesDownloadDir

[FailMsg alone does not mark history entry Failed]
file internal/postproc/stages.go
--- anchor
	return j.ParError || j.UnpackError || j.FailMsg != ""
--- replace
	return j.ParError || j.UnpackError
--- end

[the history entry ignores a failed job]
file internal/app/history_helper.go
--- anchor
	if ppJob.Failed() {
--- replace
	if false {
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
