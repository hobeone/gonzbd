pkg ./internal/dispatch/
run TestDispatcherControlSurface_PerJobDoors|TestDispatcherRemove_IsIdempotentAndReturnsResources

[PauseJob also sets the queue-wide flag]
file internal/dispatch/registry.go
--- anchor
	if err := j.SetIntent(job.IntentPause); err != nil {
		return fmt.Errorf("dispatch: pause %s: %w", j.ID(), err)
--- replace
	d.q.Pause()
	if err := j.SetIntent(job.IntentPause); err != nil {
		return fmt.Errorf("dispatch: pause %s: %w", j.ID(), err)
--- end

[Remove deregisters before cancelling, stranding the lease and slot]
file internal/dispatch/registry.go
--- anchor
	if err := d.Cancel(id); err != nil {
--- replace
	rm.end()
	if err := d.Cancel(id); err != nil {
--- end
