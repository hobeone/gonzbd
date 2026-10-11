pkg ./internal/job/
run TestSetNameAndRecordFileFilename_OneWins$

# Job.SetName and Job.RecordFileFilename each refuse when the other has
# already landed (#838). Each refusal is reverted separately.

[RecordFileFilename ignores a rename]
file internal/job/content.go
--- anchor
	if j.name != name {
--- replace
	if false {
--- end

[SetName ignores a recorded filename]
file internal/job/job.go
--- anchor
	if j.DownloadBegun() { // takes contentMu, the inner lock
--- replace
	if false {
--- end
