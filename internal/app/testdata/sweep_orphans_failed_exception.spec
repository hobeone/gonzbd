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

# The record a retry resumes from is written_articles plus job_files; a FAILED
# history entry keeps both.
[a failed history entry no longer keeps its written_articles]
file internal/durability/reclaim.go
--- anchor
	{name: "written_articles", keptForFailedEntry: true},
--- replace
	{name: "written_articles"},
--- end

[a failed history entry no longer keeps its job_files]
file internal/durability/reclaim.go
--- anchor
	{name: "job_files", keptForFailedEntry: true},
--- replace
	{name: "job_files"},
--- end
