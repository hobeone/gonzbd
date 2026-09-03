# The red check for the residency boundary, which stopped being enforced by
# the compiler when the content tier moved into internal/job.
#
#     go run ./scripts/mutate internal/dispatch/testdata/manifest_boundary.spec
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
