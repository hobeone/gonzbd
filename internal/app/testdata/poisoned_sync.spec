pkg ./internal/app/
run TestLooseRecord_PoisonedSyncReturnsArticlesToOutstanding|TestLooseRecord_CloseTimeFsyncFaultUntrusts

# A Sync-poisoned article was marked Done by the recorder when its WriteAt
# returned. The failed fsync untrusts its file, which is what clears that Done
# bit and withdraws the buffered row; OnArticlesUnwritten only clears Emitted.

[the close-handles arm does not untrust a file whose fsync failed]
file internal/assembler/assembler.go
--- anchor
				closeErr = errors.Join(closeErr, cerr)
				a.noteFileUntrusted(k.jobID, k.fileIdx)
--- replace
				closeErr = errors.Join(closeErr, cerr)
--- end

[the untrust leaves the articles Done in memory]
file internal/app/record.go
--- anchor
	if err := j.UntrustFile(fileIdx); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end

[the untrust does not purge the file's buffered rows]
file internal/app/record.go
--- anchor
	if fv.DeleteAll || len(fv.DeleteArtIdxs) > 0 {
--- replace
	if false {
--- end

[worker exit does not untrust a file whose fsync failed]
file internal/assembler/assembler.go
--- anchor
		if err := a.syncAndClose(f); err != nil {
			a.noteFileUntrusted(k.jobID, k.fileIdx)
		}
--- replace
		if err := a.syncAndClose(f); err != nil {
			_ = k
		}
--- end
