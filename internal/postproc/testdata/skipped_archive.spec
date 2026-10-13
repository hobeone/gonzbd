pkg ./internal/postproc/
run ^(TestUnpackStage_DisabledArchiveType_IsDelivered|TestUnpackHelpers)$

# extractArchive owns the "skipped" verdict: an archive no extractor ran on
# is neither extracted nor failed, so its parts are not queued for deletion
# and finalize delivers them (#943). archiveTypes.allows is the one reading
# of the four enable_* toggles it consults (#947).

[the disabled-type gate is dropped]
file internal/postproc/stage_unpack.go
--- anchor
	if !types.allows(a.Type) {
--- replace
	if false {
--- end

[enable_unrar is ignored]
file internal/postproc/stage_unpack.go
--- anchor
		return at.rar
--- replace
		return true
--- end

[enable_7zip is ignored]
file internal/postproc/stage_unpack.go
--- anchor
		return at.sevenZip
--- replace
		return true
--- end

[enable_filejoin is ignored]
file internal/postproc/stage_unpack.go
--- anchor
		return at.fileJoin
--- replace
		return true
--- end

[enable_tar is ignored]
file internal/postproc/stage_unpack.go
--- anchor
		return at.tar
--- replace
		return true
--- end

[Apply drops enable_unrar]
file internal/postproc/stage_unpack.go
--- anchor
archiveTypes{rar: c.EnableUnrar,
--- replace
archiveTypes{rar: true,
--- end

[Apply drops enable_7zip]
file internal/postproc/stage_unpack.go
--- anchor
sevenZip: c.Enable7zip,
--- replace
sevenZip: true,
--- end

[a disabled type reported as attempted]
file internal/postproc/stage_unpack.go
--- anchor
		logf(ctx, log, job, slog.LevelInfo, "Skipping %s archive (disabled): %s", archiveTypeName(a.Type), filepath.Base(a.MainFile))
		return unpack.Result{}, false, nil
--- replace
		logf(ctx, log, job, slog.LevelInfo, "Skipping %s archive (disabled): %s", archiveTypeName(a.Type), filepath.Base(a.MainFile))
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
