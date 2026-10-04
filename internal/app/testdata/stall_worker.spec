pkg ./internal/app/
run ^TestStall_LeavesALiveAssessingWorkerAlone$

# Stall's pause releases only a Fetching worker; the release is
# Dispatcher.PauseJob's. The first mutation is the shape Stall's own release
# had: a yield by ID, which parks whatever worker the job has now.

[the pause yields by ID, whatever the state]
file internal/dispatch/registry.go
--- anchor
	if err := d.YieldedFrom(j, job.Fetching); err != nil && !errors.Is(err, ErrStaleReport) {
--- replace
	if err := d.Yielded(j.ID()); err != nil && !errors.Is(err, ErrStaleReport) {
--- end

[the pause yields from the state the job is at]
file internal/dispatch/registry.go
--- anchor
	if err := d.YieldedFrom(j, job.Fetching); err != nil && !errors.Is(err, ErrStaleReport) {
--- replace
	if err := d.YieldedFrom(j, j.Snapshot().State.State); err != nil && !errors.Is(err, ErrStaleReport) {
--- end
