pkg ./internal/app/
run ^(TestFileTableFromRows_RefusesAGapAndADuplicate|TestHydrate_BuildsTheFileTableFromJobFilesBeforeVerifying|TestHydrate_RefusesAManifestWhoseCountDisagreesWithTheRows|TestAppResidency_HydrateThenEvict|TestVerifyAndAttach_MissingDirectoryParksTheJob|TestLooseRecord_ZeroedRangeIsRefetched)$

# A restored job has no file table until hydration builds one from its
# job_files rows, and nothing is attached until the record has been verified.

[fileTableFromRows accepts rows that skip or repeat an index]
file internal/app/residency.go
run TestFileTableFromRows_RefusesAGapAndADuplicate
--- anchor
		if row.FileIndex != i {
--- replace
		if false {
--- end

# Without the attach, AttachArticles refuses the manifest for want of a table,
# so the hydration errors rather than leaving a job with articles and no files.
[a hydration builds the table from the rows but never attaches it]
file internal/app/residency.go
run TestHydrate_BuildsTheFileTableFromJobFilesBeforeVerifying
--- anchor
	if err := j.AttachFiles(m.Files()); err != nil {
		return err
	}
--- replace
--- end

# A job that has a table is hydrated from its articles alone; sent down the
# verifying branch, its second AttachFiles is refused.
[a re-hydration of a job with a file table takes the verifying branch]
file internal/app/residency.go
run TestAppResidency_HydrateThenEvict
--- anchor
	if hasFiles {
		return j.AttachArticles(m)
	}
--- replace
	if false {
		return j.AttachArticles(m)
	}
--- end

# A verification that faults must leave the job as it found it, or the next
# hydration finds a table, takes the articles-only branch and never verifies.
[the files and articles are attached before the record is verified]
file internal/app/residency.go
run ^(TestVerifyAndAttach_MissingDirectoryParksTheJob|TestLooseRecord_ZeroedRangeIsRefetched)$
--- anchor
	res, err := verifyJobFiles(ctx, m.Files(), files, rows, locate, false)
--- replace
	if err := j.AttachContent(m); err != nil {
		return err
	}
	res, err := verifyJobFiles(ctx, m.Files(), files, rows, locate, false)
--- end
