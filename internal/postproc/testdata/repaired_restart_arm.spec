pkg ./internal/postproc/
run TestBuildDownloadFileList_RepairedJobReportsFetchedVolumesAfterRestart

[AttachContent stops seeding the restored flag, reproducing #504: the repaired job comes back with par2Recovered false and no arm of the summary switch matches]
file internal/job/content.go
--- anchor
	j.progress.restorePar2Recovered(j.restoredPar2Recovered)
--- replace
	j.progress.restorePar2Recovered(false)
--- end

[arm C2 neutered with the fixture intact, so the repaired job falls past the only arm that can speak for it and prints nothing]
file internal/postproc/filelist.go
--- anchor
	case recoveryVols > 0 && p.Par2Recovered():
--- replace
	case false:
--- end
