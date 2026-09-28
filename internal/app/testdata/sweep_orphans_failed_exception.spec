pkg ./internal/app/
run TestStart_SweepsWhatNoDepartureReclaimed|TestStart_SweepPreservesAFailedHistoryEntrysRuns
timeout 3m

[the startup sweep never runs]
file internal/app/durability.go
--- anchor
	if app.durable != nil {
		if err := app.durable.SweepOrphans(ctx); err != nil {
--- replace
	if false {
		if err := app.durable.SweepOrphans(ctx); err != nil {
--- end

[a failed history entry no longer keeps its durable_runs]
file internal/durability/reclaim.go
--- anchor
	{name: "durable_runs", keptForFailedEntry: true},
--- replace
	{name: "durable_runs"},
--- end
