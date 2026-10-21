pkg ./internal/app/
run ^(TestRestoreProgress_EveryRegisteredJobHasFilesAndProgress|TestRestoreProgress_SeedsNoDoneBit|TestRestoreProgress_SettlesAJobWithoutStructureFailed|TestRestoreProgress_NoStoreIsANoop|TestHydrate_FirstHydrationVerifiesEveryFile|TestHydrate_VerifiedJobRehydratesWithoutVerifying|TestStart_RefusesAJobCarryingProgressBeforeTheRestorePass)$
timeout 5m

# The restore pass (Application.restoreProgress): every registered job gets a
# file table and an unverified progress record before the first tick, seeded
# with no article bit, and its first hydration verifies every file.

[the restore seed marks every file Verified]
file internal/job/progress.go
run TestHydrate_FirstHydrationVerifiesEveryFile
--- anchor
		f.Verified = seed == nil
--- replace
		f.Verified = true
--- end

[the restore pass returns before attaching anything]
file internal/app/restore_progress.go
run TestRestoreProgress_EveryRegisteredJobHasFilesAndProgress
--- anchor
	if app.durable == nil {
		return nil
	}
--- replace
	if app.durable == nil || true {
		return nil
	}
--- end

[the restore pass is not run before the first tick]
file internal/app/startup_reconcile.go
run TestRestoreProgress_EveryRegisteredJobHasFilesAndProgress
--- anchor
	if err := app.restoreProgress(ctx); err != nil {
		return err
	}
--- replace
--- end

[the restore pass reads the record with no store]
file internal/app/restore_progress.go
run TestRestoreProgress_NoStoreIsANoop
--- anchor
	if app.durable == nil {
		return nil
	}
--- replace
	if false {
		return nil
	}
--- end

[the seed drops the recorded count]
file internal/app/restore_progress.go
run TestRestoreProgress_EveryRegisteredJobHasFilesAndProgress
--- anchor
			Recorded: r.Recorded,
--- replace
			Recorded: 0,
--- end

[the seed drops the complete flag]
file internal/app/restore_progress.go
run TestRestoreProgress_EveryRegisteredJobHasFilesAndProgress
--- anchor
			Complete: r.Complete,
--- replace
			Complete: false,
--- end

[a job without structure is skipped rather than settled Failed]
file internal/app/restore_progress.go
run TestRestoreProgress_SettlesAJobWithoutStructureFailed
--- anchor
		if err != nil {
			app.failUnrestorable(j, err)
		}
--- replace
		if err != nil {
			continue
		}
--- end

[a job with no job_files rows gets a table of no files]
file internal/app/restore_progress.go
run TestRestoreProgress_SettlesAJobWithoutStructureFailed
--- anchor
		if err == nil && len(seeds) == 0 {
--- replace
		if false {
--- end

[a job without structure is not settled]
file internal/app/restore_progress.go
run TestRestoreProgress_SettlesAJobWithoutStructureFailed
--- anchor
	if err := app.dispatcher.SettleFailed(j); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end

[a job without structure gets no operational error]
file internal/app/restore_progress.go
run TestRestoreProgress_SettlesAJobWithoutStructureFailed
--- anchor
app.dispatcher.SetOperationalError(j.ID(), msg))
--- replace
app.dispatcher.SetOperationalError(j.ID(), msg[:0]))
--- end

[a job holding a table before the restore pass is accepted]
file internal/app/restore_progress.go
run TestStart_RefusesAJobCarryingProgressBeforeTheRestorePass
--- anchor
		if j.Files() != nil {
--- replace
		if false {
--- end

[the hydration branches on the file table instead of the Verified flag]
file internal/app/residency.go
run TestHydrate_FirstHydrationVerifiesEveryFile
--- anchor
	if j.AllVerified() {
		return j.AttachArticles(m)
	}
--- replace
	if ft != nil {
		return j.AttachArticles(m)
	}
--- end

[a verified job's rehydration verifies it again]
file internal/app/residency.go
run TestHydrate_VerifiedJobRehydratesWithoutVerifying
--- anchor
	if j.AllVerified() {
		return j.AttachArticles(m)
	}
--- replace
	if false {
		return j.AttachArticles(m)
	}
--- end

[the restore seed sets a Done bit for each recorded article]
file internal/job/progress.go
run TestRestoreProgress_SeedsNoDoneBit
--- anchor
		f.Recorded = seed[fi].Recorded
--- replace
		f.Recorded = seed[fi].Recorded
		for k := 0; k < seed[fi].Recorded; k++ {
			p.done.Set(p.fileStarts[fi] + k)
		}
--- end
