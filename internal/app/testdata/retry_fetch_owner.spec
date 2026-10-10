pkg ./internal/app/
run TestLooseRecord_RestoresTheStoredFetchPolicy|TestRetryHistoryJob_ConfigurationIsHonoured|TestRetryHistoryJob_PriorRulingDoesNotSurvive|TestRetryHistoryJob_SurvivesEviction|TestAddJob_FreshRecoveryVolumeSurvivesEviction|TestMaybeReleaseRecoveryVolumes_MarksThePolicyForCheckpointing|TestRetryHistoryJob_ResumesCompletedFilesFromTheRecord

# A job's par2 fetch policy is derived at construction, from live config. Every
# mutation below reintroduces one of the ways a value that was NOT derived --
# a previous attempt's verdict, or a seed default -- reached a live job instead
# (#329).
#
# The four sites are separable and each is pinned by a different test, which is
# the point: they are four distinct doors onto one invariant, not one guard
# written four times.

# Hydration is the one case where the persisted policy IS the current truth, so
# residency must restore it. Dropping this call reverts every hydrated job to
# the FetchAlways zero value, re-activating volumes the oracle had ruled out.
#
# Neutered rather than deleted so job.FetchPolicy keeps the "job" import used
# and the tree still builds -- a COMPILE_ERROR says nothing about the test.
[hydration does not restore the persisted policy]
file internal/app/residency.go
--- anchor
			_ = j.RestoreFetchPolicy(fi, job.FetchPolicy(f.FetchPolicy))
--- replace
			_ = job.FetchPolicy(f.FetchPolicy)
--- end

# The retry path must NOT apply the retained policy: it is the failed attempt's
# verdict, computed against contents the retry is about to change. The FAILED
# entry keeps its job_files rows, so the retry's verification reads that
# policy and must not install it.
[the retry path applies a policy it did not derive]
file internal/app/app.go
--- anchor
	return installVerification(j, files, rows, res, false, app.log), nil
--- replace
	return installVerification(j, files, rows, res, true, app.log), nil
--- end

# Correcting memory is not enough: the persisted row is a second door, which
# a restart's hydration reads. The retry commits every file's state through
# the recorder before the job is registered.
[the retry does not commit the corrected row before requeueing]
file internal/app/app.go
--- anchor
		if err := app.recorder.apply(context.Background(), j, nil, retryFileStates(j, finished)...); err != nil {
--- replace
		if err := error(nil); err != nil {
--- end

# A verdict that moves the policy without marking the job is undone by the next
# restart, whose hydration restores the row. The two
# call sites are mutated separately: the clean verdict's sits in its switch arm
# and the repair verdict's in releaseRecoveryVolumes, and one being pinned says
# nothing about the other.
#
# The verdict call is left in place and only the mark removed, which is exactly
# the pre-fix state -- the policy moves in memory and nothing records it.
[the clean verdict does not mark the job for checkpointing]
file internal/app/app.go
--- anchor
		_ = j.DiscardDeferredPar2()
		app.markFetchPolicyDirty(j)
--- replace
		_ = j.DiscardDeferredPar2()
--- end

[the repair verdict does not mark the job for checkpointing]
file internal/app/app.go
--- anchor
	app.markFetchPolicyDirty(j)
	return len(idxs), nil
--- replace
	return len(idxs), nil
--- end

# The ingest path's instance, and the unconditional one: seedJobFiles wrote a
# hardcoded 0 (FetchAlways) while BuildIngestJob had already put FetchIfNeeded
# in memory, so the row disagreed from the moment it existed -- for every
# on-demand-par2 job, with no retry involved.
#
# The policy handed to the store is mutated, NOT the SQL. Changing the
# statement's placeholder count produces a SQLite argument-count error, which
# is red for a reason that proves nothing about the test.
[the seed writes the default instead of the derived policy]
file internal/app/app.go
--- anchor
		policies[i] = uint8(fetch(i))
--- replace
		policies[i] = 0
--- end
