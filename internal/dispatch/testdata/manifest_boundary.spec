# The red check for the residency boundary, which stopped being enforced by
# the compiler when the content tier moved into internal/job.
#
#     go run ./scripts/mutate internal/dispatch/testdata/manifest_boundary.spec
pkg ./internal/dispatch/
run TestDispatchNamesNoManifestType

[a manifest reference is introduced via the package-qualified type]
file internal/dispatch/tick.go
--- anchor
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
--- replace
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
	var _ *job.Manifest
--- end

[reconcileResidency reads manifest contents through the *job.Job it already holds, via j.Manifest() rather than the package-qualified type]
file internal/dispatch/tick.go
--- anchor
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
--- replace
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
	if m, err := j.Manifest(); err == nil {
		_ = m.NumArticles()
	}
--- end
