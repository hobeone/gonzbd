pkg ./internal/app/
run ^(TestCleanUnrecordedRetryFiles|TestVerifyRetry_CleansUnrecordedRetryFilesAndStageDirs)$

[unrecorded files and staging dirs are not cleaned during verifyRetry]
file internal/app/app.go
--- anchor
	cleanUnrecordedRetryFiles(j.Name(), m.Files(), files, app.pipeline.sanitizeOptions(), locate, app.log)
--- replace
--- end

[unpack staging dirs are not cleaned up during retry verification]
file internal/app/verify.go
--- anchor
	unpack.CleanupStageDirs(loc.Dir)
--- replace
--- end

[unrecorded matching files are not removed]
file internal/app/verify.go
--- anchor
		if e.Type().IsRegular() && unrecordedSubjects[e.Name()] {
--- replace
		if e.Type().IsRegular() && false {
--- end

[all unrecorded files are removed including non-manifest files]
file internal/app/verify.go
--- anchor
		if e.Type().IsRegular() && unrecordedSubjects[e.Name()] {
--- replace
		if e.Type().IsRegular() {
--- end
