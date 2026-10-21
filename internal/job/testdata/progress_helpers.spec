pkg ./internal/job/
run TestMarkNotDone_RefusesAPermanentlyFailedArticle|TestFileMetaFromTable_ProjectsCountsBytesAndPar2|TestDerivedRemainingBytes_TracksCompletionAndFetchPolicy|TestPar2ReleaseReason_SetAndClear|TestSizeFigures_ExcludesDifferOnCompleteOnly

[markNotDone stops refusing a permanently failed article]
file internal/job/progress.go
--- anchor
	if !p.done.Get(i) || p.failed.Get(i) {
		return false
	}
	p.clearDone(fi, i)
--- replace
	if !p.done.Get(i) {
		return false
	}
	p.clearDone(fi, i)
--- end

[the par2 classification is dropped from the file projection]
file internal/job/progress.go
--- anchor
			IsPar2:       isPar2File(ft.Subject(fi)),
--- replace
			IsPar2:       false,
--- end

[clearPar2ReleaseReason stops clearing]
file internal/job/progress.go
--- anchor
func (p *JobProgress) clearPar2ReleaseReason() {
	p.par2ReleaseReason = ""
--- replace
func (p *JobProgress) clearPar2ReleaseReason() {
	p.par2ReleaseReason = p.par2ReleaseReason
--- end

[sizeFigures counts a file the job is not fetching]
file internal/job/progress.go
--- anchor
		if f.Fetch != FetchAlways {
			continue
		}
		expected += f.Bytes
--- replace
		if false {
			continue
		}
		expected += f.Bytes
--- end
