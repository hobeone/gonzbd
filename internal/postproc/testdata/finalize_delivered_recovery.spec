pkg ./internal/postproc/
run TestPreCheck_AlreadyDeliveredPerJobFinalDir

[pre-check ignores already-delivered per-job FinalDir]
file internal/postproc/postproc.go
--- anchor
			if errors.Is(err, fs.ErrNotExist) && job.alreadyDelivered() {
--- replace
			if false && errors.Is(err, fs.ErrNotExist) && job.alreadyDelivered() {
--- end

[pre-check treats empty existing DownloadDir as delivered]
file internal/postproc/postproc.go
--- anchor
			if errors.Is(err, fs.ErrNotExist) && job.alreadyDelivered() {
--- replace
			if (true || errors.Is(err, fs.ErrNotExist)) && job.alreadyDelivered() {
--- end

[pre-check treats non-ENOENT ReadDir error on DownloadDir as delivered]
file internal/postproc/postproc.go
--- anchor
			if errors.Is(err, fs.ErrNotExist) && job.alreadyDelivered() {
--- replace
			if (err != nil || errors.Is(err, fs.ErrNotExist)) && job.alreadyDelivered() {
--- end

[delivered recovery builds preamble log before redirecting DownloadDir]
file internal/postproc/postproc.go
--- anchor
				job.DownloadDir = job.FinalDir
				stages = stagesAfterFinalize(stages)
--- replace
				job.StageLog = append(job.StageLog, buildPreambleLog(job)...)
				job.DownloadDir = job.FinalDir
				stages = stagesAfterFinalize(stages)
--- end

[delivered recovery leaves DownloadDir at missing path]
file internal/postproc/postproc.go
--- anchor
				job.DownloadDir = job.FinalDir
				stages = stagesAfterFinalize(stages)
--- replace
				_ = job.FinalDir
				stages = stagesAfterFinalize(stages)
--- end

[delivered recovery runs finalize and earlier stages again]
file internal/postproc/postproc.go
--- anchor
				job.DownloadDir = job.FinalDir
				stages = stagesAfterFinalize(stages)
--- replace
				job.DownloadDir = job.FinalDir
				_ = stagesAfterFinalize(stages)
--- end

[alreadyDelivered accepts flat layout FinalDir]
file internal/postproc/postproc.go
--- anchor
	if j.FlatLayout || j.FinalDir == "" {
--- replace
	if j.FinalDir == "" {
--- end

[alreadyDelivered accepts empty FinalDir]
file internal/postproc/postproc.go
--- anchor
	return err == nil && len(entries) > 0
--- replace
	return err == nil && len(entries) >= 0
--- end

[processJob omits buildPreambleLog]
file internal/postproc/postproc.go
--- anchor
	job.StageLog = append(job.StageLog, buildPreambleLog(job)...)
--- replace
	_ = buildPreambleLog
--- end

[pre-check runs on FailMsg-preset job and redirects DownloadDir]
file internal/postproc/postproc.go
--- anchor
	if job.FailMsg == "" && job.DownloadDir != "" {
--- replace
	if job.DownloadDir != "" {
--- end

[processJob skips buildPreambleLog on FailMsg-preset job]
file internal/postproc/postproc.go
--- anchor
	job.StageLog = append(job.StageLog, buildPreambleLog(job)...)
--- replace
	if job.FailMsg == "" {
		job.StageLog = append(job.StageLog, buildPreambleLog(job)...)
	}
--- end

