pkg ./internal/postproc/
run TestFinalizeHelpers/moveFileByFile_source_removal_failure_is_reported

[moveFileByFile reports a failed source removal as a success]
file internal/postproc/stage_finalize.go
--- anchor
	if err := fsutil.RemoveAll(job.DownloadDir); err != nil {
--- replace
	if err := fsutil.RemoveAll(job.DownloadDir); false && err != nil {
--- end
