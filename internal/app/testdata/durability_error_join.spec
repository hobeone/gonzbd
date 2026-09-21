pkg ./internal/app/
run TestDropJobDurability_ReportsBothOwnersFailures

# The join was described by the doc comment and asserted by nothing: before the
# independence and both-fail assertions were added, every mutation below
# SURVIVED. That is why this spec exists rather than a note saying the errors
# are joined.

[an early return on the run-store failure, so failed_articles is never reached]
file internal/app/durability.go
--- anchor
	if err := app.durable.DiscardRuns(ctx, jobID); err != nil {
		errs = append(errs, fmt.Errorf("durable runs: %w", err))
	}
--- replace
	if err := app.durable.DiscardRuns(ctx, jobID); err != nil {
		return fmt.Errorf("durable runs: %w", err)
	}
--- end

[the second failure dropped on the floor, so a caller is told about one of two]
file internal/app/durability.go
--- anchor
		errs = append(errs, fmt.Errorf("failed articles: %w", err))
--- replace
		_ = err
--- end
