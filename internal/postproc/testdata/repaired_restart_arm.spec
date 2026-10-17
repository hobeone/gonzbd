pkg ./internal/postproc/
run TestBuildDownloadFileList_RepairedJobReportsFetchedVolumesAfterRestart

[AttachContent stops seeding the restored reason, reproducing #504: the repaired job comes back with no release reason, fetched is 0, and no arm of the summary switch matches]
file internal/job/content.go
--- anchor
	j.progress.restorePar2ReleaseReason(j.restoredPar2Reason)
--- replace
	j.progress.restorePar2ReleaseReason("")
--- end

[the fetched arm neutered with the fixture intact, so the repaired job falls past the only arm that can speak for it and prints nothing]
file internal/postproc/filelist.go
--- anchor
	case fetchedVols > 0:
--- replace
	case false:
--- end
