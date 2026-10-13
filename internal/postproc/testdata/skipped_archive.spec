pkg ./internal/postproc/
run ^(TestUnpackStage_DisabledArchiveType_IsDelivered|TestUnpackHelpers)$

# extractArchive owns the "skipped" verdict: an archive no extractor ran on
# is neither extracted nor failed, so its parts are not queued for deletion
# and finalize delivers them (#943).

[disabled file join reported as attempted]
file internal/postproc/stage_unpack.go
--- anchor
			logf(ctx, log, job, slog.LevelInfo, "Skipping file join (disabled): %s", filepath.Base(a.MainFile))
			return unpack.Result{}, false, nil
--- replace
			logf(ctx, log, job, slog.LevelInfo, "Skipping file join (disabled): %s", filepath.Base(a.MainFile))
			return unpack.Result{}, true, nil
--- end

[disabled tar reported as attempted]
file internal/postproc/stage_unpack.go
--- anchor
			logf(ctx, log, job, slog.LevelInfo, "Skipping tar archive (disabled): %s", filepath.Base(a.MainFile))
			return unpack.Result{}, false, nil
--- replace
			logf(ctx, log, job, slog.LevelInfo, "Skipping tar archive (disabled): %s", filepath.Base(a.MainFile))
			return unpack.Result{}, true, nil
--- end

[unrecognised type reported as attempted]
file internal/postproc/stage_unpack.go
--- anchor
		logf(ctx, log, job, slog.LevelInfo, "Skipping unrecognised archive type: %s", filepath.Base(a.MainFile))
		return unpack.Result{}, false, nil
--- replace
		logf(ctx, log, job, slog.LevelInfo, "Skipping unrecognised archive type: %s", filepath.Base(a.MainFile))
		return unpack.Result{}, true, nil
--- end

[extractors reported as skipped]
file internal/postproc/stage_unpack.go
--- anchor
	return res, true, err
--- replace
	return res, false, err
--- end

[skip sets extractedAny]
file internal/postproc/stage_unpack.go
--- anchor
		if !attempted {
--- replace
		if !attempted {
			extractedAny = true
--- end

[extractPendingArchives ignores the skip verdict]
file internal/postproc/stage_unpack.go
--- anchor
		if !attempted {
--- replace
		if !attempted && false {
--- end
