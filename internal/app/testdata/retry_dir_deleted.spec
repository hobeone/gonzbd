pkg ./internal/app/
run Test(RetryHistoryJob_AfterDownloadDirDeleted|VerifyJobFiles_RetryTreatsAMissingDirectoryAsGone|VerifyJobFiles_MissingDirectoryIsAFault)$

# A retry of a job whose download directory is gone drops the recorded rows
# and refetches; a hydration of the same job still parks on the fault.

[a retry faults on the missing directory as a hydration does]
file internal/app/verify.go
--- anchor
		if _, sErr := os.Stat(dir); sErr != nil && (!retry || !errors.Is(sErr, fs.ErrNotExist)) {
--- replace
		if _, sErr := os.Stat(dir); sErr != nil {
--- end

[a hydration treats a missing directory as absence]
file internal/app/verify.go
--- anchor
		if _, sErr := os.Stat(dir); sErr != nil && (!retry || !errors.Is(sErr, fs.ErrNotExist)) {
--- replace
		if _, sErr := os.Stat(dir); sErr != nil && !errors.Is(sErr, fs.ErrNotExist) {
--- end
