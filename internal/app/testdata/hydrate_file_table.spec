pkg ./internal/app/
run ^(TestFileTableFromRows_RefusesAGapAndADuplicate|TestRestoreProgress_BuildsTheFileTableFromJobFiles|TestHydrate_RefusesAManifestWhoseCountDisagreesWithTheRows|TestAppResidency_HydrateThenEvict|TestVerifyAndAttach_MissingDirectoryParksTheJob|TestLooseRecord_ZeroedRangeIsRefetched)$

# The restore pass builds a restored job's file table from its job_files rows
# and attaches it unverified; the job's first hydration verifies the record
# before it attaches the articles.

[fileTableFromRows accepts rows that skip or repeat an index]
file internal/app/restore_progress.go
run TestFileTableFromRows_RefusesAGapAndADuplicate
--- anchor
		if row.FileIndex != i {
--- replace
		if false {
--- end

[the restore pass builds the table from the rows but never attaches it]
file internal/app/restore_progress.go
run TestRestoreProgress_BuildsTheFileTableFromJobFiles
--- anchor
			err = j.AttachFiles(ft, seedOf(seeds))
--- replace
			_ = ft
--- end

# A restored job holds a table from the start, so a hydration that branched on
# the table would never verify it.
[the hydration branches on the file table instead of the Verified flag]
file internal/app/residency.go
run TestRestoreProgress_BuildsTheFileTableFromJobFiles
--- anchor
	if j.AllVerified() {
		_, err := j.AttachArticles(m, nil)
		return err
	}
--- replace
	if ft != nil {
		_, err := j.AttachArticles(m, nil)
		return err
	}
--- end

# A verification that faults must leave the job as it found it, or the job is
# resident on articles nothing verified.
[the articles are attached before the record is verified]
file internal/app/residency.go
run ^(TestVerifyAndAttach_MissingDirectoryParksTheJob|TestLooseRecord_ZeroedRangeIsRefetched)$
# The mutation moves the attach above the verification.
--- anchor
	res, err := verifyJobFiles(ctx, m.Files(), files, rows, locate, false)
	if err != nil {
		f := storagefault.Classify("verify", "", err)
		if vf, ok := errors.AsType[*errVerifyFault](err); ok {
			f = storagefault.Classify("verify", vf.File, vf.Err)
		}
		return r.fault(ctx, j, f, err)
	}
	if len(res.Verdicts) > 0 {
		if err := r.commit(ctx, j, res.Verdicts); err != nil {
			return r.fault(ctx, j, storagefault.Classify("commit verdicts", "", err), err)
		}
	}
	if j.State().State == job.Fetching {
		cleanUnrecordedFetchingFiles(j.Name(), files, locate, r.log)
	}
	// The message is a fact about the job, not the file: only the peek that
	// moved the job to Blocked returns one, and the first completion queued
	// may be an unflagged file's, which would file the job without the names.
	// So the first message goes to every completion of this hydration.
	var failMsg string
	var peek func(int)
	if r.peek != nil {
		peek = func(fi int) {
			if msg := r.peek(j, fi); failMsg == "" {
				failMsg = msg
			}
		}
	}
	finished, err := installVerification(j, m, files, rows, res, true, r.log, peek)
--- replace
	if _, err := j.AttachArticles(m, nil); err != nil {
		return err
	}
	res, err := verifyJobFiles(ctx, m.Files(), files, rows, locate, false)
	if err != nil {
		f := storagefault.Classify("verify", "", err)
		if vf, ok := errors.AsType[*errVerifyFault](err); ok {
			f = storagefault.Classify("verify", vf.File, vf.Err)
		}
		return r.fault(ctx, j, f, err)
	}
	if len(res.Verdicts) > 0 {
		if err := r.commit(ctx, j, res.Verdicts); err != nil {
			return r.fault(ctx, j, storagefault.Classify("commit verdicts", "", err), err)
		}
	}
	if j.State().State == job.Fetching {
		cleanUnrecordedFetchingFiles(j.Name(), files, locate, r.log)
	}
	// The message is a fact about the job, not the file: only the peek that
	// moved the job to Blocked returns one, and the first completion queued
	// may be an unflagged file's, which would file the job without the names.
	// So the first message goes to every completion of this hydration.
	var failMsg string
	var peek func(int)
	if r.peek != nil {
		peek = func(fi int) {
			if msg := r.peek(j, fi); failMsg == "" {
				failMsg = msg
			}
		}
	}
	finished, err := installVerification(j, nil, files, rows, res, true, r.log, peek)
--- end

# A first hydration cancelled while it reads the articles must report the
# cancellation, which the tick does not settle, rather than a success that
# leaves the job unverified and not resident.
[a hydration cancelled during the article read reports success]
file internal/app/residency.go
run TestHydrate_CancelledDuringArticleReadAttachesNothing
--- anchor
	arts, err := r.readArticles(ctx, id)
	if err != nil {
		return fmt.Errorf("residency: hydrate %s: %w", id, err)
	}
--- replace
	arts, err := r.readArticles(ctx, id)
	if err != nil {
		return nil
	}
--- end
