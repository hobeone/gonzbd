pkg ./internal/app/
run TestRestoreFailedDir_RefusesAnEntryRecordedUnderAnotherBase

[the other-base guard neutered]
file internal/app/retry_failed_dir.go
--- anchor
	if recordedPath != "" && filepath.Dir(recordedPath) != downloadDir {
--- replace
	if false {
--- end
