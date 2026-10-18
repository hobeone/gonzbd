pkg ./internal/postproc/
run ^TestExtractedRepairStage_NativeEngineSkipsPendingDeletions$

[go_par2 handed the unfiltered directory listing instead of repairSets' candidates]
file internal/postproc/stage_repair.go
--- anchor
	res, err := par2.GoRepair(ctx, log, main, dataFiles, func(line string) {
--- replace
	allFiles, _ := par2.CandidateFiles(job.DownloadDir)
	res, err := par2.GoRepair(ctx, log, main, allFiles, func(line string) {
--- end

[repairSets stops excluding pending deletions from the candidates]
file internal/postproc/stage_repair.go
--- anchor
	dataFiles = slices.DeleteFunc(dataFiles, job.isPendingDeletion)
--- replace
	_ = slices.DeleteFunc[[]string, string]
--- end
