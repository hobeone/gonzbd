pkg ./internal/postproc/
run TestFailedJob_DeletingStagesKeepItsFiles|TestFinalizeStage_MoveFailureSetsFailMsg|TestPostProc_HelperMethods|TestFinalizeStage_FailedFilesAccessibleForRetry

[a failed job's files are deleted anyway]
file internal/postproc/postproc.go
--- anchor
	if _, deletes := stage.(payloadDeleter); deletes && job.Failed() {
--- replace
	if _, deletes := stage.(payloadDeleter); false && deletes && job.Failed() {
--- end

[a fail message alone is not a failure]
file internal/postproc/stages.go
--- anchor
	return j.ParError || j.UnpackError || j.FailMsg != ""
--- replace
	return j.ParError || j.UnpackError
--- end

[a repair failure is not a failure]
file internal/postproc/stages.go
--- anchor
	return j.ParError || j.UnpackError || j.FailMsg != ""
--- replace
	return j.UnpackError || j.FailMsg != ""
--- end

[an unpack failure is not a failure]
file internal/postproc/stages.go
--- anchor
	return j.ParError || j.UnpackError || j.FailMsg != ""
--- replace
	return j.ParError || j.FailMsg != ""
--- end

[sample_cleanup is not a payload deleter]
file internal/postproc/sample_cleanup.go
--- anchor
func (*SampleCleanupStage) deletesPayload() {}
--- replace
func (*SampleCleanupStage) deletesPayloadUnmarked() {}
--- end

[extension_cleanup is not a payload deleter]
file internal/postproc/extension_cleanup.go
--- anchor
func (*ExtensionCleanupStage) deletesPayload() {}
--- replace
func (*ExtensionCleanupStage) deletesPayloadUnmarked() {}
--- end

[unwanted_cleanup is not a payload deleter]
file internal/postproc/unwanted_cleanup.go
--- anchor
func (*UnwantedCleanupStage) deletesPayload() {}
--- replace
func (*UnwantedCleanupStage) deletesPayloadUnmarked() {}
--- end

[the summary reads a failed job as completed]
file internal/postproc/postproc.go
--- anchor
	if job.Failed() {
		finalStatus = "Failed"
--- replace
	if false {
		finalStatus = "Failed"
--- end

[finalize delivers a failed job]
file internal/postproc/stage_finalize.go
--- anchor
	if job.Failed() {
		return f.handleFailure(ctx, log, job, folderRename)
--- replace
	if false {
		return f.handleFailure(ctx, log, job, folderRename)
--- end
