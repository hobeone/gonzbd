pkg ./internal/dispatch/
run TestDispatchNamesNoManifestType

[a manifest reference is introduced]
file internal/dispatch/tick.go
--- anchor
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
--- replace
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
	var _ *job.Manifest
--- end
