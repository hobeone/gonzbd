pkg ./internal/app/
run TestRegisterFile_RenameBetweenNameReadAndFilenameWrite$|TestRegisterFile_GivesUpAfterRenamesKeepWinning$

# A rename accepted between registerFile's read of the job's name and its
# recording of the file's filename must leave the file under the new name
# (#838). The hooked test runs the rename in that window.

[the filename is recorded whatever the name now is]
file internal/job/content.go
--- anchor
	if j.name != name {
--- replace
	if false {
--- end

[registerFile does not retry a stale name]
file internal/app/pipeline.go
--- anchor
		if !errors.Is(err, job.ErrNameChanged) || attempt == registerNameAttempts {
--- replace
		if true {
--- end

[registerFile retries past its bound]
file internal/app/pipeline.go
--- anchor
		if !errors.Is(err, job.ErrNameChanged) || attempt == registerNameAttempts {
--- replace
		if !errors.Is(err, job.ErrNameChanged) || attempt == registerNameAttempts+1 {
--- end
