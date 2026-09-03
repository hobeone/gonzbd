pkg ./internal/downloader/
run TestApplyDispatchPlan_DisconnectsDuringPropagationWindow

[a propagation-delayed article counts as downloadable anyway]
file internal/downloader/dispatch.go
--- anchor
		if opts.propagationDelay > 0 && opts.now.Before(a.jobAdded.Add(opts.propagationDelay)) {
--- replace
		if false {
--- end
