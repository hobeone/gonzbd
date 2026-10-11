pkg ./internal/app/
run TestLooseRecord_|TestHandleFileUntrusted_ReturnsArticlesToOutstandingWhenSQLiteFails|TestResidencyFault_|TestVerifyAndAttach_|TestInstallVerification_|TestVerifyRetry_|TestHydratePausedJobs_|TestRecorder_|TestDurability_DoneMeansWrittenAndRecorded|TestRecord_FlushesOnCleanShutdown|TestFinalize_

# Restart and retry resume from the loose article record (written_articles +
# job_files). Each mutation removes one half of that path; each must be caught
# by a test that reads state, not timing.

# Verification decides what is Done before the content is attached: a job
# whose files cannot be read is parked with nothing attached, so nothing
# unverified is ever served to the scheduler.
[the content is attached before the record is verified]
file internal/app/residency.go
--- anchor
	files, rows, err := r.readRecord(ctx, j.ID())
	if err != nil {
		return r.fault(ctx, j, storagefault.Classify("read record", "", err), err)
	}
--- replace
	if err := j.AttachContent(m); err != nil {
		return err
	}
	files, rows, err := r.readRecord(ctx, j.ID())
	if err != nil {
		return r.fault(ctx, j, storagefault.Classify("read record", "", err), err)
	}
--- end

# An untrusted file's rows must leave SQLite, not only memory: otherwise the
# next start trusts bytes whose fsync failed.
[an untrust clears the file in memory only]
file internal/app/record.go
--- anchor
	if err := app.recorder.apply(ctx, j, []durability.FileVerdict{
		{FileIdx: fileIdx, DeleteAll: true, ClearComplete: true},
	}); err != nil {
--- replace
	if err := ctx.Err(); err != nil {
--- end

# The finalizer's flush runs before RemoveJob, while this instance is still
# the dispatcher's; after it the instance check drops the buffered rows, so a
# FAILED entry's retry would start from less than was written.
[the finalizer does not flush before the instance leaves the dispatcher]
file internal/app/job_finalizer.go
--- anchor
		_ = app.recorder.flush(flushCtx) // flush logs its own failure
--- replace
		_ = flushCtx
--- end

# The hand-over's synchronous flush: every row is in SQLite before
# post-processing can change the bytes it describes.
[the hand-over to post-processing does not flush]
file internal/app/app.go
--- anchor
	err := app.recorder.flush(flushCtx)
	flushCancel()
	if err != nil {
		app.log.Warn("recorder flush at the hand-over to post-processing failed", "job", j.ID(), "err", err)
--- replace
	err := error(nil)
	_ = flushCtx
	flushCancel()
	if err != nil {
		app.log.Warn("recorder flush at the hand-over to post-processing failed", "job", j.ID(), "err", err)
--- end

# A replaced instance (a retry under the same ID) must not write its buffered
# rows over the record the retry verified.
[the recorder writes the rows of an instance the dispatcher no longer holds]
file internal/app/record.go
--- anchor
	return r.current(j.ID()) == j
--- replace
	return r.current(j.ID()) == j || true
--- end

# A completion the verifier produced at hydration names a file the previous
# process already finished; DirectUnpack is not fed it.
[a resumed completion is fed to DirectUnpack]
file internal/app/app.go
--- anchor
		if pp := app.config.GetPostProc(); pp.DirectUnpack && pp.EnableUnrar && !fc.Resumed {
--- replace
		if pp := app.config.GetPostProc(); pp.DirectUnpack && pp.EnableUnrar {
--- end

# Design item 1: a complete=1 file is trusted unread, so every article of its
# range without a row is failed (the complement), not left Outstanding.
[a complete file leaves its rowless articles Outstanding]
file internal/job/verification.go
--- anchor
		_ = p.markFailed(m, i) // a no-op for an article a row just marked Done
--- replace
		_ = i
--- end

# Design item 2: the resident rows of a completed file are released once its
# CRC is settled. A complete=1 file's CRC is settled from its rows at install.
[a complete file's CRC is not settled at install]
file internal/job/verification.go
--- anchor
	if v.Complete || v.Settle {
--- replace
	if v.Settle {
--- end

# Design item 3: a row that cannot be placed costs its own article, never the
# file's or the job's install (Standing Design Rule 3).
[a row that cannot be placed fails the whole install]
file internal/job/verified.go
--- anchor
			dropped++
			continue
--- replace
			return nil, 0, fmt.Errorf("row %d cannot be placed", r.ArtIdx)
--- end

# Hydration restores the stored fetch policy; a retry keeps the one it derived.
[a retry installs the stored fetch policy]
file internal/app/residency.go
--- anchor
			RestorePolicy: restorePolicy,
--- replace
			RestorePolicy: true,
--- end

# A retry reads every file, complete=1 or not: post-processing may have changed
# the bytes since the flag was written.
[a retry trusts complete=1 unread]
file internal/app/app.go
--- anchor
		files[i].Complete = false
--- replace
		_ = i
--- end

# A failed SQLite untrust still returns the file's articles to Outstanding.
[a failed SQLite untrust skips the in-memory half]
file internal/app/record.go
--- anchor
		app.log.Error("could not remove an untrusted file's record; the next start reads its rows back and checks each CRC before trusting them",
			"job", jobID, "fileidx", fileIdx, "err", err)
	}
--- replace
		app.log.Error("could not remove an untrusted file's record; the next start reads its rows back and checks each CRC before trusting them",
			"job", jobID, "fileidx", fileIdx, "err", err)
		return
	}
--- end

# A cancelled hydration is not a device fault: it parks nothing.
[a cancelled hydration parks the job]
file internal/app/residency.go
--- anchor
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
--- replace
	if false {
--- end

# A job restored at Fetching under a pause is hydrated, and so verified,
# before the first tick, so its progress reads what the record holds.
[a paused job is not hydrated at startup]
file internal/app/startup_reconcile.go
--- anchor
		if err := app.dispatcher.LoadProgress(ctx, row.ID); err != nil {
--- replace
		if err := ctx.Err(); err != nil {
--- end
