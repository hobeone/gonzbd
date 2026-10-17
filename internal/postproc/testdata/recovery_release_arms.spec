pkg ./internal/postproc/
run ^(TestBuildDownloadFileList_Par2Summary|TestBuildDownloadFileList_RepairedJobReportsFetchedVolumesAfterRestart|TestBuildDownloadFileList_RestoredVerdictIsNotReportedClean)$

# The Par2 summary of a staged release: how many of the recovery volumes were
# fetched, and what a job that fetched none, or lost content, may be called.

[a job that lost content with no verdict reads as verified clean]
file internal/postproc/filelist.go
--- anchor
	case heldVols > 0 && p.ContentFailedBytes() == 0:
--- replace
	case heldVols > 0 && true:
--- end

[a zero-deficit release (reason set, all held) falls through to the clean arm]
file internal/postproc/filelist.go
--- anchor
	case heldVols > 0 && p.HasPar2Verdict():
--- replace
	case heldVols > 0 && p.HasPar2Verdict() && false:
--- end

[the fetched arm sits behind the could-not-verify arm]
file internal/postproc/filelist.go
--- anchor
	case fetchedVols > 0:
--- replace
	case fetchedVols > 0 && heldVols == 0:
--- end

[the fetched arm is gone]
file internal/postproc/filelist.go
--- anchor
	case fetchedVols > 0:
--- replace
	case false:
--- end

[the fetched line counts the held volumes as the total]
file internal/postproc/filelist.go
--- anchor
			fetchedVols, recoveryVols, p.Par2ReleaseReason()))
--- replace
			fetchedVols, heldVols+0*recoveryVols, p.Par2ReleaseReason()))
--- end

# RecoveryReleased reports fetched only once a reason is recorded. With
# on-demand par2 off every volume is FetchAlways from ingest, and the summary
# must stay silent.

[a FetchAlways volume counts as fetched with no reason recorded]
file internal/job/progress.go
--- anchor
	if p.par2ReleaseReason != "" {
		fetched = total - held
--- replace
	if p.par2ReleaseReason != "" || true {
		fetched = total - held
--- end
