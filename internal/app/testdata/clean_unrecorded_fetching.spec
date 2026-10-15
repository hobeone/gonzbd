pkg ./internal/app/
run ^(TestVerifyAndAttach_RemovesUnrecordedFilesAndStageDirsWhenFetching|TestVerifyAndAttach_PreservesExtractedFilesWhenNotFetching)$

[unrecorded files and staging dirs are not cleaned when fetching]
file internal/app/residency.go
--- anchor
	if j.State().State == job.Fetching {
		cleanUnrecordedFetchingFiles(j.Name(), files, locate, r.log)
	}
--- replace
	if false {
		cleanUnrecordedFetchingFiles(j.Name(), files, locate, r.log)
	}
--- end

[unrecorded files are cleaned unconditionally even when not fetching]
file internal/app/residency.go
--- anchor
	if j.State().State == job.Fetching {
		cleanUnrecordedFetchingFiles(j.Name(), files, locate, r.log)
	}
--- replace
	if true {
		cleanUnrecordedFetchingFiles(j.Name(), files, locate, r.log)
	}
--- end

[unpack staging dirs are not cleaned up during hydration]
file internal/app/verify.go
--- anchor
	unpack.CleanupStageDirs(jobDir)
--- replace
	if false {
		unpack.CleanupStageDirs(jobDir)
	}
--- end

[unrecorded files are not removed]
file internal/app/verify.go
--- anchor
		if e.Type().IsRegular() && !recorded[e.Name()] {
--- replace
		if e.Type().IsRegular() && false {
--- end

[recorded files are removed along with unrecorded files]
file internal/app/verify.go
--- anchor
		if e.Type().IsRegular() && !recorded[e.Name()] {
--- replace
		if e.Type().IsRegular() {
--- end
