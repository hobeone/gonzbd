pkg ./internal/job/
run TestSetNameAndRecordFileFilename_OneWins$|TestFileLocation$

# Job.SetName and Job.RecordFileFilename each refuse when the other has
# already landed (#838). Each refusal is reverted separately. FileLocation is
# the one read of the name and a filename that registerFile resolves from.

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

[FileLocation drops the recorded filename]
file internal/job/content.go
--- anchor
	return j.name, j.progress.FileFilename(fileIdx)
--- replace
	return j.name, ""
--- end
