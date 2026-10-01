pkg ./internal/app/
run TestCheckpointJob_ACommitErrorStallsTheJobUntilReevaluated|TestHandleFileComplete_AFinalizeCommitErrorStallsNamingTheStore|TestHandleFileComplete_AFinalizeCommitAbandonedByItsCallerIsNotStalled

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

[the application's store is not given the path history.Open was called with]
file internal/app/app.go
--- anchor
		durStore = durability.NewStore(repo.DB(), repo.Path())
--- replace
		durStore = durability.NewStore(repo.DB(), "history.db")
--- end
