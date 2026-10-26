pkg ./internal/assembler/
run ^Test(CloseJobHandles_UntrustsAFileWhoseCloseTimeSyncFailed|SyncAndCloseAll_UntrustsAFileWhoseSyncFailed|OptionsSyncFile_ReachesTheCompletionFsync|Sync_ActuallyIssuesTheFsync)$

# A failed fsync releases its file's written articles for refetch through
# OnFileUntrusted, once per file, and through nothing else.

[the close-handles arm does not untrust a file whose fsync failed]
file internal/assembler/assembler.go
--- anchor
				closeErr = errors.Join(closeErr, cerr)
				a.noteFileUntrusted(f.owner, k.fileIdx)
--- replace
				closeErr = errors.Join(closeErr, cerr)
--- end

[the close-handles arm untrusts a failed file twice]
file internal/assembler/assembler.go
--- anchor
				closeErr = errors.Join(closeErr, cerr)
				a.noteFileUntrusted(f.owner, k.fileIdx)
--- replace
				closeErr = errors.Join(closeErr, cerr)
				a.noteFileUntrusted(f.owner, k.fileIdx)
				a.noteFileUntrusted(f.owner, k.fileIdx)
--- end

[worker exit does not untrust a file whose fsync failed]
file internal/assembler/assembler.go
--- anchor
		if err := a.syncAndClose(f); err != nil {
			a.noteFileUntrusted(f.owner, k.fileIdx)
		}
--- replace
		if err := a.syncAndClose(f); err != nil {
			_ = k
		}
--- end

[a failed completion finish does not untrust its file]
file internal/assembler/assembler.go
--- anchor
		a.noteFileUntrusted(req.Owner, req.FileIdx)
--- replace
		_ = req
--- end

[noteFileUntrusted does not reach its owner]
file internal/assembler/assembler.go
--- anchor
		a.opts.OnFileUntrusted(owner, fileIdx)
--- replace
		_, _ = owner, fileIdx
--- end

[sync swallows a failed fsync]
file internal/assembler/filewriter.go
--- anchor
	if err := w.syncFile(); err != nil {
--- replace
	if err := w.syncFile(); false && err != nil {
--- end

[openTargetFile omits wiring Options.SyncFile]
file internal/assembler/assembler.go
--- anchor
	if syncFn := a.opts.SyncFile; syncFn != nil {
--- replace
	if syncFn := a.opts.SyncFile; false && syncFn != nil {
--- end
