pkg ./internal/assembler/
run Test(Sync_ActuallyIssuesTheFsyncAndKeepsTheReport|DrainReport_FailedSyncPoisonsReportAndRollsBackArticles|Assembler_FailedSyncReleasesArticlesToOutstandingAndAcksNothingOnRetry|BarrierWithAssembler_FailedSyncAcksNothingOnRetryAndReturnsArticlesToOutstanding|FileWriter_PoisonSyncAndRollbackSyncedArticle|DrainAndClose_FailedSyncRoutesRolledBackArticles)$

[Sync omits poisonSync on fsync error]
file internal/assembler/filewriter.go
--- anchor
	if err := w.syncFile(); err != nil {
		w.poisonSync()
		return storagefault.Classify("sync", w.path, err)
	}
--- replace
	if err := w.syncFile(); err != nil {
		return storagefault.Classify("sync", w.path, err)
	}
--- end

[poisonSync omits rolling back w.reported]
file internal/assembler/filewriter.go
--- anchor
	for _, a := range w.reported {
		w.rollbackSyncedArticle(a.ArtIdx)
	}
--- replace
	for range w.reported {
	}
--- end

[poisonSync omits rolling back w.written]
file internal/assembler/filewriter.go
--- anchor
	for _, a := range w.written {
		w.rollbackSyncedArticle(a.ArtIdx)
	}
--- replace
	for range w.written {
	}
--- end

[poisonSync retains w.reported instead of clearing it]
file internal/assembler/filewriter.go
--- anchor
	w.reported = nil
	w.written = nil
--- replace
	w.written = nil
--- end

[poisonSync retains w.written instead of clearing it]
file internal/assembler/filewriter.go
--- anchor
	w.reported = nil
	w.written = nil
--- replace
	w.reported = nil
--- end

[rollbackSyncedArticle omits duplicate check on w.poisoned]
file internal/assembler/filewriter.go
--- anchor
	if slices.Contains(w.poisoned, artIdx) {
		return
	}
--- replace
	if false && slices.Contains(w.poisoned, artIdx) {
		return
	}
--- end

[rollbackSyncedArticle omits recording the article in w.poisoned]
file internal/assembler/filewriter.go
--- anchor
	w.fail(articleID{artIdx: artIdx})
	w.poisoned = append(w.poisoned, artIdx)
--- replace
	w.fail(articleID{artIdx: artIdx})
--- end

[opSync omits releaseSyncRollback after Sync]
file internal/assembler/synctarget.go
--- anchor
		case opSync:
			r.err = f.w.Sync()
			// A failed Sync poisons the retained report and rolls its articles
			// back into w.poisoned (#760); route them back to Outstanding and
			// lift any completed tombstone if partsWritten dropped below
			// TotalParts.
			a.releaseSyncRollback(f, key, completed)
--- replace
		case opSync:
			r.err = f.w.Sync()
--- end

[opTruncate omits ErrFileIncomplete check when rolledBack]
file internal/assembler/synctarget.go
--- anchor
		case opTruncate:
			if f.rolledBack {
				r.err = fmt.Errorf("assembler: job %s file %d is incomplete (%d/%d parts): %w",
					op.jobID, op.fileIdx, f.w.parts(), f.info.TotalParts, durability.ErrFileIncomplete)
				break
			}
			r.err = f.w.Truncate(op.bound)
--- replace
		case opTruncate:
			r.err = f.w.Truncate(op.bound)
--- end

[opClose omits releaseSyncRollback before delete]
file internal/assembler/synctarget.go
--- anchor
			r.err = a.drainAndClose(f)
			a.releaseSyncRollback(f, key, completed)
			delete(open, key)
--- replace
			r.err = a.drainAndClose(f)
			delete(open, key)
--- end

[releaseSyncRollback fails to lift completed tombstone when parts rolled back below TotalParts]
file internal/assembler/synctarget.go
--- anchor
	if f.info.TotalParts > 0 && f.w.parts() < f.info.TotalParts {
		if _, wasComplete := completed[key]; wasComplete {
			delete(completed, key)
			f.rolledBack = true
		}
	}
--- replace
	if false && f.info.TotalParts > 0 && f.w.parts() < f.info.TotalParts {
		if _, wasComplete := completed[key]; wasComplete {
			delete(completed, key)
			f.rolledBack = true
		}
	}
--- end

[releaseSyncRollback deletes completed tombstone even when parts still meet TotalParts]
file internal/assembler/synctarget.go
--- anchor
	if f.info.TotalParts > 0 && f.w.parts() < f.info.TotalParts {
--- replace
	if f.info.TotalParts > 0 && f.w.parts() <= f.info.TotalParts {
--- end

[releaseSyncRollback deletes completed tombstone when TotalParts is zero]
file internal/assembler/synctarget.go
--- anchor
	if f.info.TotalParts > 0 && f.w.parts() < f.info.TotalParts {
--- replace
	if f.info.TotalParts >= 0 && (f.info.TotalParts == 0 || f.w.parts() < f.info.TotalParts) {
--- end

[finalizeFile omits clearing rolledBack when re-completed]
file internal/assembler/assembler.go
--- anchor
func (a *Assembler) finalizeFile(f *openFile, key fileKey, req WriteRequest, completed map[fileKey]struct{}) {
	completed[key] = struct{}{} // tombstone: reject late duplicates
	f.rolledBack = false
--- replace
func (a *Assembler) finalizeFile(f *openFile, key fileKey, req WriteRequest, completed map[fileKey]struct{}) {
	completed[key] = struct{}{} // tombstone: reject late duplicates
--- end

[openTargetFile omits wiring Options.SyncFile]
file internal/assembler/assembler.go
--- anchor
	if a.opts.SyncFile != nil {
		f.w.syncFile = a.opts.SyncFile
	}
--- replace
	if false && a.opts.SyncFile != nil {
		f.w.syncFile = a.opts.SyncFile
	}
--- end

[drainAndClose omits releasePoisoned after failed Sync]
file internal/assembler/assembler.go
--- anchor
		a.releasePoisoned(f)
	}
	// A failing Close is a storage condition too, and on network-backed mounts
--- replace
	}
	// A failing Close is a storage condition too, and on network-backed mounts
--- end

[releaseSyncRollback omits routing the poisoned set]
file internal/assembler/synctarget.go
--- anchor
	a.releasePoisoned(f)
	if f.info.TotalParts > 0 && f.w.parts() < f.info.TotalParts {
--- replace
	if f.info.TotalParts > 0 && f.w.parts() < f.info.TotalParts {
--- end
