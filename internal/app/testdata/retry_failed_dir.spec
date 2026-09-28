pkg ./internal/app/
run Test(RetryHistoryJob_ResumesInTheFailedDirectory|RestoreFailedDir|RetryHistoryJob_AbortMovesTheFailedDirectoryBack|RetryHistoryJob_RefusesWhenTheJobDirectoryExists)$

# A failed job's download directory, renamed _FAILED_<name> by the finalize
# stage, is moved back to downloadDir/<name> before the retry is queued, and
# moved back again if the retry aborts.

[restoreFailedDir never recognises the _FAILED_ path]
file internal/app/retry_failed_dir.go
--- anchor
	if recordedPath != failedDir {
--- replace
	if true {
--- end

[RetryHistoryJob never restores the directory]
file internal/app/app.go
--- anchor
	restoredFrom, err := restoreFailedDir(entry.Path, downloadDir, j.Name(), app.queuedName)
--- replace
	restoredFrom, err := "", error(nil)
--- end

[an aborted retry leaves the directory restored]
file internal/app/app.go
--- anchor
		if restoreKept {
--- replace
		if restoreKept || true {
--- end

[an admitted retry moves the directory back anyway]
file internal/app/app.go
--- anchor
	restoreKept = true
--- replace
	restoreKept = false
--- end

[an existing job directory does not refuse the retry]
file internal/app/retry_failed_dir.go
--- anchor
	case dstExists:
--- replace
	case false:
--- end

[a queued job of the same name does not refuse the retry]
file internal/app/retry_failed_dir.go
--- anchor
	case nameQueued(name):
--- replace
	case false:
--- end

[a _FAILED_ path that is not a directory is moved anyway]
file internal/app/retry_failed_dir.go
--- anchor
	case !srcInfo.IsDir():
--- replace
	case srcInfo == nil:
--- end

[a vanished _FAILED_ directory with a job directory present is accepted]
file internal/app/retry_failed_dir.go
--- anchor
		return "", fmt.Errorf("%w: %s is gone and %s exists, and may belong to another job",
			errRetryDirConflict, failedDir, jobDir)
--- replace
		return "", nil
--- end

[undo is a no-op]
file internal/app/retry_failed_dir.go
--- anchor
	return renameWithin(downloadDir, filepath.Join(downloadDir, name), from)
--- replace
	return nil
--- end
