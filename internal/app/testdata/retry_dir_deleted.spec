pkg ./internal/app/
run Test(RetryHistoryJob_AfterDownloadDirDeleted|VerifyAndAttach_MissingDirectoryParksTheJob|VerifyJobFiles_RetryTreatsAMissingDirectoryAsGone|VerifyJobFiles_MissingDirectoryIsAFault|VerifyJobFiles_RetryFaultsOnAnotherDirectoryOpenError)$

# A retry of a job whose download directory is gone drops the recorded rows
# and refetches; a hydration of the same job still parks on the fault. An
# error opening the directory other than ENOENT is a fault on both paths.

[a retry faults on the missing directory as a hydration does]
file internal/app/verify.go
--- anchor
		if retry && errors.Is(err, fs.ErrNotExist) {
--- replace
		if false {
--- end

[a hydration treats a missing directory as absence]
file internal/app/verify.go
--- anchor
		if retry && errors.Is(err, fs.ErrNotExist) {
--- replace
		if errors.Is(err, fs.ErrNotExist) {
--- end

[a retry treats any directory open error as absence]
file internal/app/verify.go
--- anchor
		if retry && errors.Is(err, fs.ErrNotExist) {
--- replace
		if retry {
--- end

[a hydration verifies as a retry does]
file internal/app/residency.go
--- anchor
	res, err := verifyJobFiles(ctx, m, files, rows, locate, false)
--- replace
	res, err := verifyJobFiles(ctx, m, files, rows, locate, true)
--- end
