pkg ./internal/app/
run ^(TestMaybeReleaseRecoveryVolumes_SizedRelease|TestAppRunner_RunAssessBranches)$

# An assessment cancelled by shutdown acts on no verdict, releases no
# recovery volume, and yields the job at Assessing with its deferred reasons
# kept (#1008).

[a verdict reached under cancellation is acted on]
file internal/app/app.go
--- anchor
	// reached under cancellation would release every held volume. None is
	// acted on; runAssess yields the job at Assessing.
	if ctx.Err() != nil {
--- replace
	// reached under cancellation would release every held volume. None is
	// acted on; runAssess yields the job at Assessing.
	if false {
--- end

[a cancelled assessment advances]
file internal/app/runner.go
--- anchor
	if ctx.Err() != nil {
		// Shutdown cancels ctx
--- replace
	if false {
		// Shutdown cancels ctx
--- end

[a cancelled assessment hands its deferred reasons over]
file internal/app/runner.go
--- anchor
		handOver = false
--- replace
		_ = handOver
--- end

[a cancelled assessment yields by ID, not this instance]
file internal/app/runner.go
--- anchor
r.report.YieldedJob(j))
--- replace
r.report.Yielded(id))
--- end
