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

[rollbackSyncedArticle omits unlatching written on w.accepted]
file internal/assembler/filewriter.go
--- anchor
		if w.accepted[i].id.artIdx == artIdx {
			w.accepted[i].written = false
			id = w.accepted[i].id
		}
--- replace
		if w.accepted[i].id.artIdx == artIdx {
			id = w.accepted[i].id
		}
--- end

[rollbackSyncedArticle omits duplicate check on w.faulted]
file internal/assembler/filewriter.go
--- anchor
		if !f.displaced && f.id.artIdx == artIdx {
			return
		}
--- replace
		if false && !f.displaced && f.id.artIdx == artIdx {
			return
		}
--- end

[rollbackSyncedArticle treats displaced faulted entry as unwritten duplicate]
file internal/assembler/filewriter.go
--- anchor
		if !f.displaced && f.id.artIdx == artIdx {
			return
		}
--- replace
		if f.id.artIdx == artIdx {
			return
		}
--- end

[opDrain omits releaseSyncRollback after Drain]
file internal/assembler/synctarget.go
--- anchor
		case opDrain:
			r.written, r.err = f.w.Drain()
			// The barrier routes the fault; it cannot route the ARTICLES. A
			// failed drain rolls back every article after the write that
			// failed, and that set never crosses the SyncTarget interface —
			// so without this they are neither Done, nor Failed, nor
			// Outstanding, and only a restart recovers them.
			a.releaseSyncRollback(f, key, completed)
--- replace
		case opDrain:
			r.written, r.err = f.w.Drain()
			a.releaseFaulted(f, key.jobID, key.fileIdx)
--- end

[opSync omits releaseSyncRollback after Sync]
file internal/assembler/synctarget.go
--- anchor
		case opSync:
			r.err = f.w.Sync()
			// A failed Sync poisons the retained report and rolls its articles
			// back into w.faulted (#760); route them back to Outstanding and
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

[drainAndClose omits releaseFaulted after failed Sync]
file internal/assembler/assembler.go
--- anchor
	if syncErr := f.w.Sync(); syncErr != nil {
		note("sync file before close", syncErr)
		a.releaseFaulted(f, key.jobID, key.fileIdx)
	}
--- replace
	if syncErr := f.w.Sync(); syncErr != nil {
		note("sync file before close", syncErr)
	}
--- end
