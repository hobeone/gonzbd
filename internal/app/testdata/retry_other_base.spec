pkg ./internal/app/
run TestCheckRecordedUnderCurrentBase

[the other-base guard neutered]
file internal/app/retry_failed_dir.go
--- anchor
	if recordedPath == "" || filepath.Dir(filepath.Clean(recordedPath)) == filepath.Clean(downloadDir) {
--- replace
	if true {
--- end

[the configured download_dir compared uncleaned]
file internal/app/retry_failed_dir.go
--- anchor
	if recordedPath == "" || filepath.Dir(filepath.Clean(recordedPath)) == filepath.Clean(downloadDir) {
--- replace
	if recordedPath == "" || filepath.Dir(filepath.Clean(recordedPath)) == downloadDir {
--- end

[complete_dir entries refused]
file internal/app/retry_failed_dir.go
--- anchor
	if completeDir != "" {
--- replace
	if false {
--- end
