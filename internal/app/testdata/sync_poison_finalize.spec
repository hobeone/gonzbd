pkg ./internal/app/
run ^TestReevaluateStall_FailedFinalizeSyncRollsBackAndRetrimsOnRedelivery$

[reevaluateStall omits ErrFileIncomplete case on retryFinalize]
file internal/app/stall.go
--- anchor
		case errors.Is(err, durability.ErrFileIncomplete):
			// A failed Drain or Sync rolled back one or more of this file's
			// articles to Outstanding and lifted the assembler's completed
			// tombstone (#760). Keep the open FileWriter handle in place and
			// drop the pending finalize entry: once Phase 2 resumes the job,
			// re-fetching the Outstanding articles will reach TotalParts on
			// the same FileWriter and fire OnFileComplete afresh.
			app.dropPendingFinalize(jobID, fileIdx)
			delete(files, fileIdx)
--- replace
		case false && errors.Is(err, durability.ErrFileIncomplete):
--- end

[reevaluateStall omits dropPendingFinalize on ErrFileIncomplete]
file internal/app/stall.go
--- anchor
			app.dropPendingFinalize(jobID, fileIdx)
			delete(files, fileIdx)
--- replace
			delete(files, fileIdx)
--- end

[reevaluateStall omits deleting fileIdx from local files map on ErrFileIncomplete]
file internal/app/stall.go
--- anchor
			app.dropPendingFinalize(jobID, fileIdx)
			delete(files, fileIdx)
--- replace
			app.dropPendingFinalize(jobID, fileIdx)
--- end

[routeFinalizeFailure classifies ErrFileIncomplete as a storage fault]
file internal/app/durability.go
--- anchor
	if errors.Is(err, durability.ErrFileIncomplete) {
		app.log.Info("completed file rolled back to incomplete before finalization; "+
			"it will finalize again once re-fetched articles arrive",
			"job", jobID, "fileidx", fileIdx, "err", err)
		return
	}
--- replace
	if false && errors.Is(err, durability.ErrFileIncomplete) {
		return
	}
--- end

[Barrier.raise classifies ErrFileIncomplete as a storage fault]
file internal/durability/barrier.go
--- anchor
	if errors.Is(err, ErrFileNotOpen) || errors.Is(err, ErrTargetUnavailable) || errors.Is(err, ErrFileIncomplete) {
--- replace
	if errors.Is(err, ErrFileNotOpen) || errors.Is(err, ErrTargetUnavailable) {
--- end

[Barrier.FinalizeFile skips t.Truncate when bound <= 0]
file internal/durability/barrier.go
--- anchor
	if err := t.Truncate(ctx, idx, truncBound); err != nil {
		if errors.Is(err, ErrFileNotOpen) {
			b.log.Debug("file closed before its finalize could truncate",
				"job", jobID, "file", idx)
			return nil
		}
		return b.raise(jobID, "truncate", t.Path(idx), err)
	}
--- replace
	if bound > 0 {
		if err := t.Truncate(ctx, idx, truncBound); err != nil {
			if errors.Is(err, ErrFileNotOpen) {
				b.log.Debug("file closed before its finalize could truncate",
					"job", jobID, "file", idx)
				return nil
			}
			return b.raise(jobID, "truncate", t.Path(idx), err)
		}
	}
--- end
