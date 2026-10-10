pkg ./internal/postproc/
run ^TestDeferredCleanup_|^TestPendingDeletionHelpers$

[deobfuscate skips deduplicating re-extracted obfuscated file on rerun]
file internal/deobfuscate/deobfuscate.go
--- anchor
	if usefulName != "" {
		paths, relPaths = dedupObfuscatedAgainstTarget(log, root, usefulName, paths, relPaths, opts)
	}
--- replace
	if false && usefulName != "" {
		paths, relPaths = dedupObfuscatedAgainstTarget(log, root, usefulName, paths, relPaths, opts)
	}
--- end

[deobfuscate deduplicates non-obfuscated file against target]
file internal/deobfuscate/deobfuscate.go
--- anchor
		if relDst != rel && IsProbablyObfuscated(log, rel) {
--- replace
		if relDst != rel && (true || IsProbablyObfuscated(log, rel)) {
--- end

[deobfuscate deduplicates obfuscated file without checking content equality]
file internal/deobfuscate/deobfuscate.go
--- anchor
				if eq, err := streamEqual(root, rel, relDst); err == nil && eq && root.Remove(rel) == nil {
--- replace
				if _, err := streamEqual(root, rel, relDst); err == nil && root.Remove(rel) == nil {
--- end

[unpack cleanupArchives deletes immediately instead of deferring]
file internal/postproc/stage_unpack.go
--- anchor
			for _, part := range a.Parts {
				if _, err := os.Lstat(part); err == nil && job.recordPendingDeletion(part) {
--- replace
			for _, part := range a.Parts {
				if err := os.Remove(part); err == nil {
--- end

[repairSets does not exclude pending deletions from dataFiles]
file internal/postproc/stage_repair.go
--- anchor
	dataFiles = slices.DeleteFunc(dataFiles, job.isPendingDeletion)
--- replace
	_ = slices.DeleteFunc[[]string, string]
--- end

[par2_cleanup deletes main par2 immediately instead of deferring]
file internal/postproc/stage_par2cleanup.go
--- anchor
		if set.MainFile != "" {
			if _, err := os.Lstat(set.MainFile); err == nil && job.recordPendingDeletion(set.MainFile) {
--- replace
		if set.MainFile != "" {
			if err := os.Remove(set.MainFile); err == nil {
--- end

[par2_cleanup skips recording extra par2 files for deletion]
file internal/postproc/stage_par2cleanup.go
--- anchor
		for _, ef := range set.ExtraFiles {
			if _, err := os.Lstat(ef); err == nil && job.recordPendingDeletion(ef) {
--- replace
		for _, ef := range set.ExtraFiles {
			if false && job.recordPendingDeletion(ef) {
--- end

[par2_cleanup skips recording backup files for deletion]
file internal/postproc/stage_par2cleanup.go
--- anchor
	for _, backup := range backups {
		if job.recordPendingDeletion(filepath.Join(job.DownloadDir, backup)) {
--- replace
	for _, backup := range backups {
		if false && job.recordPendingDeletion(filepath.Join(job.DownloadDir, backup)) {
--- end

[sample_cleanup does not skip pending deletions]
file internal/postproc/sample_cleanup.go
--- anchor
		if d.IsDir() || path == "." || job.isPendingDeletion(path) {
--- replace
		if d.IsDir() || path == "." {
--- end

[recover_par2_names does not exclude pending deletions]
file internal/postproc/stage_par2names.go
--- anchor
	renames, err := deobfuscate.Par2RenameExcluding(ctx, log, root, job.DownloadDir, job.Sanitize, job.pendingDeletionSet())
--- replace
	renames, err := deobfuscate.Par2RenameExcluding(ctx, log, root, job.DownloadDir, job.Sanitize, nil)
--- end

[deobfuscate stage does not exclude pending deletions]
file internal/postproc/stage_deobfuscate.go
--- anchor
	exclude := job.pendingDeletionSet()
--- replace
	var exclude map[string]struct{}
--- end

[unwanted_cleanup does not skip pending deletions]
file internal/postproc/unwanted_cleanup.go
--- anchor
		if job.isPendingDeletion(path) {
			return nil
		}
--- replace
		if false && job.isPendingDeletion(path) {
			return nil
		}
--- end

[extension_cleanup does not skip pending deletions]
file internal/postproc/extension_cleanup.go
--- anchor
		if d.IsDir() || path == "." || job.isPendingDeletion(path) {
--- replace
		if d.IsDir() || path == "." {
--- end

[finalize skips deleting pending files when already at FinalDir]
file internal/postproc/stage_finalize.go
--- anchor
	if job.DownloadDir == job.FinalDir {
		logf(ctx, log, job, slog.LevelInfo, "Already at final location: %s", job.FinalDir)
		f.deletePending(ctx, log, job)
		return nil // Already there (e.g. one-shot download directly to target)
	}
--- replace
	if job.DownloadDir == job.FinalDir {
		logf(ctx, log, job, slog.LevelInfo, "Already at final location: %s", job.FinalDir)
		return nil // Already there (e.g. one-shot download directly to target)
	}
--- end

[finalize skips deleting pending files after moveToDest]
file internal/postproc/stage_finalize.go
--- anchor
	if err := os.Rename(job.DownloadDir, dest); err == nil {
		logf(ctx, log, job, slog.LevelInfo, "%s → %s (atomic rename)", job.DownloadDir, dest)
		job.DownloadDir = dest
		// Unlink pending archives and par2 files while the directory is still
		// at dest (with _UNPACK_ prefix when folderRename is enabled) before
		// publishing job.FinalDir (#768).
		f.deletePending(ctx, log, job)
--- replace
	if err := os.Rename(job.DownloadDir, dest); err == nil {
		logf(ctx, log, job, slog.LevelInfo, "%s → %s (atomic rename)", job.DownloadDir, dest)
		job.DownloadDir = dest
--- end

[finalize deletes pending files after stripping _UNPACK_ prefix instead of before]
file internal/postproc/stage_finalize.go
--- anchor
		// Unlink pending archives and par2 files while the directory is still
		// at dest (with _UNPACK_ prefix when folderRename is enabled) before
		// publishing job.FinalDir (#768).
		f.deletePending(ctx, log, job)
		// If FolderRename is active, strip the _UNPACK_ prefix now.
		if folderRename {
			if err := os.Rename(dest, job.FinalDir); err != nil {
				logf(ctx, log, job, slog.LevelWarn, "Failed to strip _UNPACK_ prefix: %v", err)
				// Not fatal — files are in _UNPACK_ dir but accessible.
			} else {
				logf(ctx, log, job, slog.LevelInfo, "%s → %s (prefix stripped)", dest, job.FinalDir)
				job.DownloadDir = job.FinalDir
			}
		}
--- replace
		if folderRename {
			if err := os.Rename(dest, job.FinalDir); err != nil {
				logf(ctx, log, job, slog.LevelWarn, "Failed to strip _UNPACK_ prefix: %v", err)
			} else {
				logf(ctx, log, job, slog.LevelInfo, "%s → %s (prefix stripped)", dest, job.FinalDir)
				job.DownloadDir = job.FinalDir
			}
		}
		f.deletePending(ctx, log, job)
--- end

[finalize skips staging via _UNPACK_ prefix when folderRename is false and PendingDeletions is non-empty]
file internal/postproc/stage_finalize.go
--- anchor
	if !stageViaUnpackPrefix && len(job.PendingDeletions) > 0 {
--- replace
	if false && !stageViaUnpackPrefix && len(job.PendingDeletions) > 0 {
--- end

[finalize deletes pending files even when moveToDest fails]
file internal/postproc/stage_finalize.go
--- anchor
	if err := f.moveToDest(ctx, log, job, dest, stageViaUnpackPrefix); err != nil {
		job.FailMsg = err.Error()
		return err
	}
--- replace
	if err := f.moveToDest(ctx, log, job, dest, stageViaUnpackPrefix); err != nil {
		f.deletePending(ctx, log, job)
		job.FailMsg = err.Error()
		return err
	}
--- end

[finalize deletePending skips PathWithin containment guard]
file internal/postproc/stage_finalize.go
--- anchor
		if !fsutil.PathWithin(job.DownloadDir, target) {
			continue
		}
--- replace
		if false && !fsutil.PathWithin(job.DownloadDir, target) {
			continue
		}
--- end

[finalize deletePending skips first-component Sanitize fallback]
file internal/postproc/stage_finalize.go
--- anchor
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			first, rest, hasSep := strings.Cut(rel, string(filepath.Separator))
			sanitized := fsutil.JoinSafe(job.DownloadDir, "", first, job.Sanitize)
			if hasSep {
				sanitized = filepath.Join(sanitized, rest)
			}
			if sanitized != target && fsutil.PathWithin(job.DownloadDir, sanitized) {
				target = sanitized
			}
		}
--- replace
		_ = job.Sanitize
--- end

[finalize moveFileByFile copies pending files instead of skipping them]
file internal/postproc/stage_finalize.go
--- anchor
		if job.isPendingDeletion(src) {
			continue
		}
--- replace
		if false && job.isPendingDeletion(src) {
			continue
		}
--- end

[finalize moveRecursive copies nested pending files instead of skipping them]
file internal/postproc/stage_finalize.go
--- anchor
	if isPending != nil && isPending(src) {
		return nil
	}
--- replace
	if false && isPending != nil && isPending(src) {
		return nil
	}
--- end

[sweepTempFiles skips CleanupStageDirs]
file internal/postproc/postproc.go
--- anchor
	unpack.CleanupStageDirs(dir)
--- replace
	_ = unpack.CleanupStageDirs
--- end

[finalize does not remove pre-existing pendingDeletionsFile in DownloadDir]
file internal/postproc/stage_finalize.go
--- anchor
	if job.DownloadDir != "" {
		_ = os.Remove(filepath.Join(job.DownloadDir, pendingDeletionsFile))
	}
--- replace
	if false && job.DownloadDir != "" {
		_ = os.Remove(filepath.Join(job.DownloadDir, pendingDeletionsFile))
	}
--- end

[finalize skips writing pendingDeletionsFile sidecar before rename]
file internal/postproc/stage_finalize.go
--- anchor
	if len(job.PendingDeletions) > 0 {
		if data, err := json.Marshal(job.PendingDeletions); err == nil {
			if err := fsutil.WriteAtomicBytesPerm(sidecar, data, 0o600); err != nil {
				logf(ctx, log, job, slog.LevelWarn, "Failed to write pending deletions sidecar: %v", err)
			}
		}
	}
--- replace
	_ = json.Marshal
--- end

[moveToDest skips logging warning when WriteAtomicBytesPerm fails]
file internal/postproc/stage_finalize.go
--- anchor
			if err := fsutil.WriteAtomicBytesPerm(sidecar, data, 0o600); err != nil {
				logf(ctx, log, job, slog.LevelWarn, "Failed to write pending deletions sidecar: %v", err)
			}
--- replace
			if err := fsutil.WriteAtomicBytesPerm(sidecar, data, 0o600); err != nil {
				_ = err
			}
--- end

[moveToDest skips removing sidecar before moveFileByFile fallback]
file internal/postproc/stage_finalize.go
--- anchor
	} else {
		_ = os.Remove(sidecar)
		logf(ctx, log, job, slog.LevelInfo, "Atomic rename failed (%v), falling back to file-by-file move", err)
	}
--- replace
	} else {
		logf(ctx, log, job, slog.LevelInfo, "Atomic rename failed (%v), falling back to file-by-file move", err)
	}
--- end

[deletePending removes pendingDeletionsFile sidecar before loop instead of after]
file internal/postproc/stage_finalize.go
--- anchor
	if len(job.PendingDeletions) == 0 || job.DownloadDir == "" {
		return
	}
	var deleted int
	for _, rel := range job.PendingDeletions {
--- replace
	if len(job.PendingDeletions) == 0 || job.DownloadDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(job.DownloadDir, pendingDeletionsFile))
	var deleted int
	for _, rel := range job.PendingDeletions {
--- end

[deletePending skips removing pendingDeletionsFile sidecar after loop]
file internal/postproc/stage_finalize.go
--- anchor
	if deleted > 0 {
		logf(ctx, log, job, slog.LevelInfo, "Cleaned up %d pending file(s)", deleted)
	}
	_ = os.Remove(filepath.Join(job.DownloadDir, pendingDeletionsFile))
--- replace
	if deleted > 0 {
		logf(ctx, log, job, slog.LevelInfo, "Cleaned up %d pending file(s)", deleted)
	}
--- end

[recoverUnpackFinalDir skips deletePending before renaming _UNPACK_ directory]
file internal/postproc/postproc.go
--- anchor
			(&FinalizeStage{Log: p.log}).deletePending(ctx, p.log, job)
--- replace
			_ = ctx
--- end

[recoverUnpackFinalDir skips renaming _UNPACK_ directory to FinalDir]
file internal/postproc/postproc.go
--- anchor
			if errors.Is(err, fs.ErrNotExist) {
				recoverErr = p.recoverUnpackFinalDir(ctx, job)
			}
--- replace
			if false && errors.Is(err, fs.ErrNotExist) {
				recoverErr = p.recoverUnpackFinalDir(ctx, job)
			}
--- end

[recoverUnpackFinalDir skips logging Info on recovery]
file internal/postproc/postproc.go
--- anchor
	p.log.Info("postproc: recovered staged _UNPACK_ directory to FinalDir",
		"job", job.JobID(),
		"unpack_dir", unpackDir,
		"final_dir", job.FinalDir,
	)
--- replace
	_ = unpackDir
--- end




