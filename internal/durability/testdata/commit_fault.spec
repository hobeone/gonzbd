pkg ./internal/durability/
run TestBarrier_Run_ACommitErrorStallsTheJob|TestBarrier_Run_ACommitAbandonedByItsCallerIsNotStalled

[Run returns a failed commit without routing it]
file internal/durability/barrier.go
--- anchor
		return nil, b.raise(jobID, "commit", "", commitFailure(ctx, err))
--- replace
		return nil, fmt.Errorf("durability: barrier commit for %s: %w", jobID, commitFailure(ctx, err))
--- end

[raise routes nothing for an error it does not recognise]
file internal/durability/barrier.go
--- anchor
	return b.routeFault(jobID, storagefault.Classify(op, path, err))
--- replace
	return fmt.Errorf("durability: barrier %s job=%s: %w", op, jobID, err)
--- end

[a commit abandoned by its caller is classified as storage]
file internal/durability/barrier.go
--- anchor
	if ctx.Err() != nil {
		return fmt.Errorf("%w: commit abandoned by its caller (%w): %w",
--- replace
	if false {
		return fmt.Errorf("%w: commit abandoned by its caller (%w): %w",
--- end
