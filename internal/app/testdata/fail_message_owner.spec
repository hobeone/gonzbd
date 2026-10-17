pkg ./internal/app/
run TestBuildHistoryEntry_FailMessageNamesTheStageError

[history reads FailMsg instead of the job's FailReason]
file internal/app/history_helper.go
--- anchor
		entry.FailMessage = ppJob.FailReason()
--- replace
		entry.FailMessage = ppJob.FailMsg
--- end

[the repair stage's error is not kept as the cause]
file internal/postproc/stages.go
--- anchor
	if j.ParError && !parBefore {
--- replace
	if false {
--- end

[the unpack stage's error is not kept as the cause]
file internal/postproc/stages.go
--- anchor
	if j.UnpackError && !unpackBefore {
--- replace
	if false {
--- end
