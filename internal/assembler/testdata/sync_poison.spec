pkg ./internal/assembler/
run Test(Sync_ActuallyIssuesTheFsync|Sync_FailedSyncRollsBackEveryUnsyncedArticle|Sync_SuccessCoversTheArticlesWrittenBeforeIt|FileWriter_PoisonSyncAndRollbackSyncedArticle|SyncAndClose_FailedSyncRoutesRolledBackArticles|OptionsSyncFile_ReachesTheCompletionFsync)$

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

[Sync success leaves the covered articles in unsynced]
file internal/assembler/filewriter.go
--- anchor
	// Covered by this fsync: a later failure says nothing about these.
	w.unsynced = nil
--- replace
	// Covered by this fsync: a later failure says nothing about these.
--- end

[poisonSync omits rolling back w.unsynced]
file internal/assembler/filewriter.go
--- anchor
	for _, a := range w.unsynced {
		w.rollbackSyncedArticle(a)
	}
--- replace
	for range w.unsynced {
	}
--- end

[poisonSync retains w.unsynced instead of clearing it]
file internal/assembler/filewriter.go
--- anchor
		w.rollbackSyncedArticle(a)
	}
	w.unsynced = nil
--- replace
		w.rollbackSyncedArticle(a)
	}
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

[openTargetFile omits wiring Options.SyncFile]
file internal/assembler/assembler.go
--- anchor
	if syncFn := a.opts.SyncFile; syncFn != nil {
--- replace
	if syncFn := a.opts.SyncFile; false && syncFn != nil {
--- end

[syncAndClose omits releasePoisoned after failed Sync]
file internal/assembler/assembler.go
--- anchor
		a.releasePoisoned(f)
	}
	// A failing Close is a storage condition too, and on network-backed mounts
--- replace
	}
	// A failing Close is a storage condition too, and on network-backed mounts
--- end
