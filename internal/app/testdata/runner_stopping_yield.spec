pkg ./internal/app/
run TestAppRunner_EveryStateDischargesCompletionContract|TestApplication_Shutdown_WedgedComponent|TestBarrierRunsOnCleanShutdown

[stopping does not yield immediately]
file internal/app/runner.go
--- anchor
	if r.app != nil && r.app.stopping.Load() {
--- replace
	if false {
--- end
