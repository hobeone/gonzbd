pkg ./internal/app/
run TestRestoreJobFiles_RestoresNonDefaultFetchPolicy|TestRetryHistoryJob_ConfigurationIsHonoured|TestRetryHistoryJob_PriorRulingDoesNotSurvive|TestRetryHistoryJob_SurvivesEviction|TestAddJob_FreshRecoveryVolumeSurvivesEviction

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
# This is the change's main risk and nothing caught it before: all four
# pre-existing tests that insert job_files rows write FetchAlways, so a dropped
# restore was invisible to every one of them.
#
# Neutered rather than deleted so job.FetchPolicy keeps the "job" import used
# and the tree still builds -- a COMPILE_ERROR says nothing about the test.
[hydration does not restore the persisted policy]
file internal/app/residency.go
--- anchor
		_ = j.RestoreFetchPolicy(fi, job.FetchPolicy(fetch)) //nolint:gosec // G115: fetch_policy is 0-2, fits in uint8
--- replace
		_ = job.FetchPolicy(fetch) //nolint:gosec // G115: fetch_policy is 0-2, fits in uint8
--- end

# The retry path must NOT apply the retained policy: it is the failed attempt's
# verdict, computed against contents the retry is about to change.
#
# The mutation reconstructs the DEFECT rather than the deleted code. Task 1
# removed three non-contiguous things from this file, so restoring it verbatim
# would need three anchors; appending one call after a surviving one is a single
# contiguous anchor, compiles, and puts the wrong value where the wrong value
# used to land.
[the retry path applies a policy it did not derive]
file internal/app/app.go
--- anchor
			_ = j.RestoreFileMeta(f.FileIndex, f.Filename, f.Complete, f.AssembledCRC32)
--- replace
			_ = j.RestoreFileMeta(f.FileIndex, f.Filename, f.Complete, f.AssembledCRC32)
			_ = j.RestoreFetchPolicy(f.FileIndex, job.FetchNever)
--- end

# Correcting memory is not enough: the persisted row is a second door. Failed
# jobs keep their job_files rows on purpose, and every hydration re-applies
# them, so without a synchronous flush an eviction between the retry and the
# first checkpoint restores the failed attempt's value.
#
# Only the Flush is neutered, not the Mark. Marking without flushing is exactly
# the pre-fix state: the job is dirty, and nothing has written the row yet.
[the retry does not flush the corrected row before requeueing]
file internal/app/app.go
--- anchor
		if err := app.checkpointer.Flush(context.Background()); err != nil {
			return fmt.Errorf("app: retry %s: flush checkpoint: %w", jobID, err)
		}
--- replace
		_ = context.Background()
--- end

# The ingest path's instance, and the unconditional one: seedJobFiles wrote a
# hardcoded 0 (FetchAlways) while BuildIngestJob had already put FetchIfNeeded
# in memory, so the row disagreed from the moment it existed -- for every
# on-demand-par2 job, with no retry involved.
#
# The argument expression is mutated, NOT the VALUES literal. Changing the
# placeholder count produces a SQLite argument-count error, which is red for a
# reason that proves nothing about the test.
[the seed writes the default instead of the derived policy]
file internal/app/app.go
--- anchor
		if _, err := stmt.ExecContext(ctx, jobID, i, int(fetch(i))); err != nil {
--- replace
		if _, err := stmt.ExecContext(ctx, jobID, i, 0); err != nil {
--- end
