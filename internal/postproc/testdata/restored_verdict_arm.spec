pkg ./internal/postproc/
run TestBuildDownloadFileList_RestoredVerdictIsNotReportedClean

[AttachContent stops seeding the restored reason, as before #504: the verdict is lost across the restart and the job reports verified clean]
file internal/job/content.go
--- anchor
	j.progress.restorePar2ReleaseReason(j.restoredPar2Reason)
--- replace
	j.progress.restorePar2ReleaseReason("")
--- end

[arm C1 neutered with the fixture intact, so the job falls through to the bare heldVols arm — this is what pins the assertions rather than the fixture guard]
file internal/postproc/filelist.go
--- anchor
	case heldVols > 0 && !p.Par2Recovered() && p.HasPar2Verdict():
--- replace
	case false:
--- end

[RestoreProgressState drops the reason, reproducing the silent no-op the old SetPar2ReleaseReason guard had at this call site]
file internal/job/content.go
--- anchor
func (j *Job) RestoreProgressState(reason string, started, finished time.Time) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.restoredPar2Reason = reason
--- replace
func (j *Job) RestoreProgressState(reason string, started, finished time.Time) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.restoredPar2Reason = ""
--- end
