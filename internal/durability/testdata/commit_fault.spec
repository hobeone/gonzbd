pkg ./internal/durability/
run TestBarrier_Run_ACommitErrorStallsTheJob|TestBarrier_Run_ACommitAbandonedByItsCallerIsNotStalled|TestBarrier_FinalizeFile_AStoreErrorStallsNamingTheStore|TestBarrier_FinalizeFile_AStoreCallAbandonedByItsCallerIsNotStalled

[the barrier's commit returns a failure without routing it]
file internal/durability/barrier.go
--- anchor
		return nil, b.raise(jobID, "commit", b.runs.Path(), storeFailure(ctx, err))
--- replace
		return nil, fmt.Errorf("durability: barrier commit for %s: %w", jobID, storeFailure(ctx, err))
--- end

[a failed commit's stall reason drops the store's path]
file internal/durability/barrier.go
--- anchor
		return nil, b.raise(jobID, "commit", b.runs.Path(), storeFailure(ctx, err))
--- replace
		return nil, b.raise(jobID, "commit", "", storeFailure(ctx, err))
--- end

[a failed commit is not checked for a caller that stopped waiting]
file internal/durability/barrier.go
--- anchor
		return nil, b.raise(jobID, "commit", b.runs.Path(), storeFailure(ctx, err))
--- replace
		return nil, b.raise(jobID, "commit", b.runs.Path(), err)
--- end

[raise routes nothing for an error it does not recognise]
file internal/durability/barrier.go
--- anchor
	return b.routeFault(jobID, storagefault.Classify(op, path, err))
--- replace
	return fmt.Errorf("durability: barrier %s job=%s: %w", op, jobID, err)
--- end

[a store call abandoned by its caller is classified as storage]
file internal/durability/barrier.go
--- anchor
	if ctx.Err() != nil {
		return fmt.Errorf("%w: store call abandoned by its caller (%w): %w",
--- replace
	if false {
		return fmt.Errorf("%w: store call abandoned by its caller (%w): %w",
--- end

[FinalizeFile's run read returns a failure without routing it]
file internal/durability/barrier.go
--- anchor
		return nil, b.raise(jobID, "read", b.runs.Path(), storeFailure(ctx, err))
--- replace
		return nil, fmt.Errorf("durability: finalize runs job=%s file=%d: %w", jobID, idx, storeFailure(ctx, err))
--- end

[FinalizeFile's failed run read names the completed file]
file internal/durability/barrier.go
--- anchor
		return nil, b.raise(jobID, "read", b.runs.Path(), storeFailure(ctx, err))
--- replace
		return nil, b.raise(jobID, "read", t.Path(idx), storeFailure(ctx, err))
--- end

[FinalizeFile's failed run read is not checked for a caller that stopped waiting]
file internal/durability/barrier.go
--- anchor
		return nil, b.raise(jobID, "read", b.runs.Path(), storeFailure(ctx, err))
--- replace
		return nil, b.raise(jobID, "read", b.runs.Path(), err)
--- end
