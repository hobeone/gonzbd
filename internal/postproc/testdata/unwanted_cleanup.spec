pkg ./internal/postproc/
run TestUnwantedCleanup_

[an approved job is cleaned anyway]
file internal/postproc/unwanted_cleanup.go
--- anchor
	if job.Unwanted == unwanted.StateApproved {
--- replace
	if false && job.Unwanted == unwanted.StateApproved {
--- end

[a failed job is cleaned anyway]
file internal/postproc/unwanted_cleanup.go
--- anchor
	if job.ParError || job.UnpackError || job.FailMsg != "" {
--- replace
	if job.ParError {
--- end

[a rules error lets the job through]
file internal/postproc/unwanted_cleanup.go
--- anchor
		job.FailMsg = fmt.Sprintf("unwanted-extension check could not run: %v", err)
--- replace
		_ = fmt.Sprintf("unwanted-extension check could not run: %v", err)
--- end

[action off is ignored]
file internal/postproc/unwanted_cleanup.go
--- anchor
	if rules.Action() == unwanted.ActionOff {
--- replace
	if false && rules.Action() == unwanted.ActionOff {
--- end

[only files OwnedFiles lists are judged]
file internal/postproc/unwanted_cleanup.go
--- anchor
		if d.IsDir() || !rules.Unwanted(d.Name()) {
--- replace
		if _, owned := job.OwnedFiles[filepath.Join(job.DownloadDir, path)]; d.IsDir() || !rules.Unwanted(d.Name()) || (job.OwnedFiles != nil && !owned) {
--- end

[nothing is removed]
file internal/postproc/unwanted_cleanup.go
--- anchor
		if d.IsDir() || !rules.Unwanted(d.Name()) {
--- replace
		if true || !rules.Unwanted(d.Name()) {
--- end

[an unopenable directory lets the job through]
file internal/postproc/unwanted_cleanup.go
--- anchor
		return s.fail(job, fmt.Errorf("open %s: %w", job.DownloadDir, err))
--- replace
		return nil
--- end

[an unreadable directory stops the walk]
file internal/postproc/unwanted_cleanup.go
--- anchor
			errs = append(errs, err)
			if d != nil && d.IsDir() {
--- replace
			return err
			if d != nil && d.IsDir() {
--- end

[a failed removal is only logged]
file internal/postproc/unwanted_cleanup.go
--- anchor
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
--- replace
			_ = fmt.Errorf("remove %s: %w", path, err)
--- end

[recorded errors do not fail the job]
file internal/postproc/unwanted_cleanup.go
--- anchor
	if err := errors.Join(errs...); err != nil {
--- replace
	if err := errors.Join(errs...); false && err != nil {
--- end

[emptied directories are left behind]
file internal/postproc/unwanted_cleanup.go
--- anchor
		cleanupEmptyDirs(root)
--- replace
		_ = root
--- end
