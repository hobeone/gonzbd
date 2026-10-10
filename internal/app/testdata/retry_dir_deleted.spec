pkg ./internal/app/
run Test(RetryHistoryJob_AfterDownloadDirDeleted|VerifyJobFiles_RetryTreatsAMissingDirectoryAsGone|VerifyJobFiles_MissingDirectoryIsAFault|VerifyJobFiles_RetryFaultsOnAnotherDirectoryStatError)$

# A retry of a job whose download directory is gone drops the recorded rows
# and refetches; a hydration of the same job still parks on the fault. A stat
# error other than ENOENT is a fault on both paths.

[a retry faults on the missing directory as a hydration does]
file internal/app/verify.go
--- anchor
		if _, sErr := statDir(dir); sErr != nil && (!retry || !errors.Is(sErr, fs.ErrNotExist)) {
--- replace
		if _, sErr := statDir(dir); sErr != nil {
--- end

[a hydration treats a missing directory as absence]
file internal/app/verify.go
--- anchor
		if _, sErr := statDir(dir); sErr != nil && (!retry || !errors.Is(sErr, fs.ErrNotExist)) {
--- replace
		if _, sErr := statDir(dir); sErr != nil && !errors.Is(sErr, fs.ErrNotExist) {
--- end

[a retry treats any stat error as absence]
file internal/app/verify.go
--- anchor
		if _, sErr := statDir(dir); sErr != nil && (!retry || !errors.Is(sErr, fs.ErrNotExist)) {
--- replace
		if _, sErr := statDir(dir); sErr != nil && !retry {
--- end
