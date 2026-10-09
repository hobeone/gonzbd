pkg ./internal/postproc/
run TestProcessJob_SweepsLeftoverTempFiles

[processJob skips sweepTempFiles]
file internal/postproc/postproc.go
--- anchor
	sweepTempFiles(p.log, job.DownloadDir)
--- replace
	_ = job.DownloadDir
--- end

[processJob runs sweepTempFiles after stages instead of before]
file internal/postproc/postproc.go
--- anchor
	sweepTempFiles(p.log, job.DownloadDir)
--- replace
	defer sweepTempFiles(p.log, job.DownloadDir)
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
