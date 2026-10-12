pkg ./internal/postproc/
run TestSABFiles|TestBuildEnv_FilesAlwaysValidJSONArray|TestPreCheck_AlreadyDeliveredPerJobFinalDir

[the sort of the recorded paths dropped]
file internal/postproc/stage_finalize.go
--- anchor
	slices.Sort(files)
--- replace
	_ = slices.Sort[[]string]
--- end

[paths recorded absolute instead of relative to the job directory]
file internal/postproc/stage_finalize.go
--- anchor
		files = append(files, filepath.ToSlash(rel))
--- replace
		_ = rel
		files = append(files, filepath.ToSlash(path))
--- end

[the walk error swallowed]
file internal/postproc/stage_finalize.go
--- anchor
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
--- replace
		if err != nil {
			return nil
		}
		if !d.Type().IsRegular() {
--- end

[directories and symlinks listed with the files]
file internal/postproc/stage_finalize.go
--- anchor
		if !d.Type().IsRegular() {
--- replace
		if false {
--- end

[a failed job's remaining files not recorded]
file internal/postproc/stage_finalize.go
--- anchor
	recordWalkedOutputFiles(job)
	logf(ctx, log, job, slog.LevelInfo, "Skipped final move
--- replace
	logf(ctx, log, job, slog.LevelInfo, "Skipped final move
--- end

[the list not re-derived when a delivered job resumes]
file internal/postproc/postproc.go
--- anchor
				recordWalkedOutputFiles(job)
				stages = stagesAfterFinalize(stages)
--- replace
				stages = stagesAfterFinalize(stages)
--- end

[an empty list encoded as null]
file internal/postproc/script.go
--- anchor
	if len(files) == 0 {
		return "[]"
	}
--- replace
--- end

[the encoded-size bound dropped]
file internal/postproc/stage_script.go
--- anchor
	if err == nil && len(encodeFiles(files)) > maxFilesEnvBytes {
--- replace
	if false {
--- end
