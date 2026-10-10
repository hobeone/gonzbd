pkg ./internal/app/
run TestRecorder_UntrustAfterInFlightFailedFlushWins

[apply no longer takes the writer lock]
file internal/app/record.go
--- anchor
func (r *recorder) apply(ctx context.Context, j *job.Job, v []durability.FileVerdict, files ...durability.FileState) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
--- replace
func (r *recorder) apply(ctx context.Context, j *job.Job, v []durability.FileVerdict, files ...durability.FileState) error {
--- end
