pkg ./internal/postproc/
run TestFinalizeStage_MoveFailureSetsFailMsg

[empty FinalDir leaves FailMsg unset]
file internal/postproc/stage_finalize.go
--- anchor
	if job.FinalDir == "" {
		err := errors.New("finalize: FinalDir not set")
		job.FailMsg = err.Error()
		return err
	}
--- replace
	if job.FinalDir == "" {
		return errors.New("finalize: FinalDir not set")
	}
--- end

[moveToDest error leaves FailMsg unset]
file internal/postproc/stage_finalize.go
--- anchor
	if err := f.moveToDest(ctx, log, job, dest, stageViaUnpackPrefix); err != nil {
		job.FailMsg = err.Error()
		return err
	}
--- replace
	if err := f.moveToDest(ctx, log, job, dest, stageViaUnpackPrefix); err != nil {
		return err
	}
--- end

[partial move omits moved file names]
file internal/postproc/stage_finalize.go
--- anchor
		moved = append(moved, e.Name())
--- replace
		_ = moved
--- end

[partial move omits unmoved file names]
file internal/postproc/stage_finalize.go
--- anchor
			failed = append(failed, e.Name())
--- replace
			_ = failed
--- end

[partial move removes source directory]
file internal/postproc/stage_finalize.go
--- anchor
	if len(moveErrors) > 0 {
		// Some files failed to move — do NOT remove the source directory
		// to avoid data loss of the unmoved files.
--- replace
	if len(moveErrors) > 0 {
		_ = fsutil.RemoveAll(job.DownloadDir)
--- end

[moveToDest unpack prefix strip failure fails the job]
file internal/postproc/stage_finalize.go
--- anchor
			if err := os.Rename(dest, job.FinalDir); err != nil {
				logf(ctx, log, job, slog.LevelWarn, "Failed to strip _UNPACK_ prefix: %v", err)
				// Not fatal — files are in _UNPACK_ dir but accessible.
			}
--- replace
			if err := os.Rename(dest, job.FinalDir); err != nil {
				return fmt.Errorf("finalize: strip prefix: %w", err)
			}
--- end

[moveFileByFile unpack prefix strip failure fails the job]
file internal/postproc/stage_finalize.go
--- anchor
	// Strip _UNPACK_ prefix if FolderRename is active.
	if folderRename {
		if err := os.Rename(dest, job.FinalDir); err != nil {
			logf(ctx, log, job, slog.LevelWarn, "Failed to strip _UNPACK_ prefix: %v", err)
		} else {
--- replace
	// Strip _UNPACK_ prefix if FolderRename is active.
	if folderRename {
		if err := os.Rename(dest, job.FinalDir); err != nil {
			return fmt.Errorf("finalize: strip prefix: %w", err)
		} else {
--- end
