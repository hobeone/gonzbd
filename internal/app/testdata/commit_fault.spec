pkg ./internal/app/
run TestCheckpointJob_ACommitErrorStallsTheJobUntilReevaluated

[Run returns a failed commit without routing it]
file internal/durability/barrier.go
--- anchor
		return nil, b.raise(jobID, "commit", b.runs.Path(), commitFailure(ctx, err))
--- replace
		return nil, fmt.Errorf("durability: barrier commit for %s: %w", jobID, commitFailure(ctx, err))
--- end

[a failed commit's stall reason drops the store's path]
file internal/durability/barrier.go
--- anchor
		return nil, b.raise(jobID, "commit", b.runs.Path(), commitFailure(ctx, err))
--- replace
		return nil, b.raise(jobID, "commit", "", commitFailure(ctx, err))
--- end

[raise routes nothing for an error it does not recognise]
file internal/durability/barrier.go
--- anchor
	return b.routeFault(jobID, storagefault.Classify(op, path, err))
--- replace
	return fmt.Errorf("durability: barrier %s job=%s: %w", op, jobID, err)
--- end
