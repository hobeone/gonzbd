pkg ./internal/app/
run TestRecorder_UntrustAfterInFlightFailedFlushWins

[apply no longer takes the writer lock]
file internal/app/record.go
--- anchor
func (r *recorder) apply(ctx context.Context, j *job.Job, v []durability.FileVerdict, files ...durability.FileState) error {
	if err := r.lockWriter(ctx); err != nil {
		return err
	}
	defer r.unlockWriter()
--- replace
func (r *recorder) apply(ctx context.Context, j *job.Job, v []durability.FileVerdict, files ...durability.FileState) error {
--- end
