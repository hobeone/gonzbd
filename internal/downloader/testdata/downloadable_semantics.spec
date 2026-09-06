pkg ./internal/downloader/
run TestBuildDispatchPlan_PropagationDelayHoldsBack

[a propagation-delayed job is dispatched anyway]
file internal/downloader/dispatch.go
--- anchor
		if opts.propagationDelay > 0 && opts.now.Before(j.Added().Add(opts.propagationDelay)) {
--- replace
		if false {
--- end
