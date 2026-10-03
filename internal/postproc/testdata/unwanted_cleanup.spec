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

[a file the job does not own is removed]
file internal/postproc/unwanted_cleanup.go
--- anchor
			if _, owned := job.OwnedFiles[absPath]; !owned {
--- replace
			if _, owned := job.OwnedFiles[absPath]; false && !owned {
--- end

[nothing is removed]
file internal/postproc/unwanted_cleanup.go
--- anchor
		if !rules.Unwanted(d.Name()) {
--- replace
		if true || !rules.Unwanted(d.Name()) {
--- end

[emptied directories are left behind]
file internal/postproc/unwanted_cleanup.go
--- anchor
		cleanupEmptyDirs(root)
--- replace
		_ = root
--- end
