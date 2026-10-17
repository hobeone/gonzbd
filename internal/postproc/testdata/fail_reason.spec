pkg ./internal/postproc/
run TestJob_FailReason|TestFinalizeHelpers

[the unpack cause is overwritten by a later stage's error]
file internal/postproc/stages.go
--- anchor
	if j.UnpackError && !unpackBefore {
--- replace
	if j.UnpackError {
--- end

[the repair cause is overwritten by a later stage's error]
file internal/postproc/stages.go
--- anchor
	if j.ParError && !parBefore {
--- replace
	if j.ParError {
--- end

[finalize's failure log line reads FailMsg instead of FailReason]
file internal/postproc/stage_finalize.go
--- anchor
job.FailReason())
--- replace
job.FailMsg)
--- end
