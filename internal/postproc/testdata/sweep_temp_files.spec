pkg ./internal/postproc/
run TestProcessJob_SweepsLeftoverTempFiles

[processJob skips sweepTempFiles]
file internal/postproc/postproc.go
--- anchor
	p.log.Info("postproc: processing job", "job", job.JobID(), "name", job.Name())

	sweepTempFiles(p.log, job.DownloadDir)
--- replace
	p.log.Info("postproc: processing job", "job", job.JobID(), "name", job.Name())

	_ = job.DownloadDir
--- end

[processJob runs sweepTempFiles after stages instead of before]
file internal/postproc/postproc.go
--- anchor
	p.log.Info("postproc: processing job", "job", job.JobID(), "name", job.Name())

	sweepTempFiles(p.log, job.DownloadDir)
--- replace
	p.log.Info("postproc: processing job", "job", job.JobID(), "name", job.Name())

	defer sweepTempFiles(p.log, job.DownloadDir)
--- end

[processJob skips sweepTempFiles on redirected FinalDir]
file internal/postproc/postproc.go
--- anchor
				stages = stagesAfterFinalize(stages)
				sweepTempFiles(p.log, job.DownloadDir)
--- replace
				stages = stagesAfterFinalize(stages)
--- end

[alreadyDelivered counts leftover temp files as delivered payload]
file internal/postproc/postproc.go
--- anchor
	entries = slices.DeleteFunc(entries, func(e os.DirEntry) bool { return fsutil.IsTempFile(e.Name()) })
--- replace
	entries = slices.DeleteFunc(entries, func(e os.DirEntry) bool { return false && fsutil.IsTempFile(e.Name()) })
--- end

[sweepTempFiles returns early unconditionally]
file internal/postproc/postproc.go
--- anchor
	if dir == "" {
--- replace
	if true || dir == "" {
--- end

[sweepTempFiles skips removing matching temp files]
file internal/postproc/postproc.go
--- anchor
		if fsutil.IsTempFile(d.Name()) {
--- replace
		if false && fsutil.IsTempFile(d.Name()) {
--- end

[sweepTempFiles matches any dotfile instead of fsutil.IsTempFile]
file internal/postproc/postproc.go
--- anchor
		if fsutil.IsTempFile(d.Name()) {
--- replace
		if strings.HasPrefix(d.Name(), ".") || fsutil.IsTempFile(d.Name()) {
--- end

[sweepTempFiles matches any .gonzbd-tmp- prefix without hex suffix check]
file internal/postproc/postproc.go
--- anchor
		if fsutil.IsTempFile(d.Name()) {
--- replace
		if strings.HasPrefix(d.Name(), fsutil.TempFilePrefix) {
--- end

[sweepTempFiles skips logging removed leftover temp file]
file internal/postproc/postproc.go
--- anchor
			if log != nil {
				log.Info("postproc: removed leftover temp file", "path", filepath.Join(dir, path))
			}
--- replace
			if false && log != nil {
				log.Info("postproc: removed leftover temp file", "path", filepath.Join(dir, path))
			}
--- end

[sweepTempFiles skips logging warning when Remove fails]
file internal/postproc/postproc.go
--- anchor
				if !errors.Is(rmErr, os.ErrNotExist) && log != nil {
					log.Warn("postproc: failed to remove leftover temp file", "path", filepath.Join(dir, path), "err", rmErr)
				}
--- replace
				if false && !errors.Is(rmErr, os.ErrNotExist) && log != nil {
					log.Warn("postproc: failed to remove leftover temp file", "path", filepath.Join(dir, path), "err", rmErr)
				}
--- end

[sweepTempFiles skips logging warning when OpenRoot fails]
file internal/postproc/postproc.go
--- anchor
		if !errors.Is(err, os.ErrNotExist) && log != nil {
			log.Warn("postproc: failed to open download dir for temp-file sweep", "dir", dir, "err", err)
		}
--- replace
		if false && !errors.Is(err, os.ErrNotExist) && log != nil {
			log.Warn("postproc: failed to open download dir for temp-file sweep", "dir", dir, "err", err)
		}
--- end
