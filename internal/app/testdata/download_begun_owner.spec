pkg ./internal/app/
run TestRenameJob_RefusedAfterAFailedFirstArticle|TestInstallVerification_FilenameWithoutRowsMarksBegun

# #825: DownloadBegun reads the file registration (a resolved filename), which
# the permanent-failure path sets, not the start stamp, which it does not.
[DownloadBegun reverted to the start stamp or a done article]
file internal/job/content.go
--- anchor
	for i := range j.progress.files {
		if j.progress.files[i].Filename != "" {
			return true
		}
	}
	return false
--- replace
	return !j.progress.downloadStarted.IsZero() || j.progress.articlesResolved > j.progress.articlesFailed
--- end

# The restart side: a recorded filename is restored even for a file with no
# written rows, so a hydrated job still reports DownloadBegun.
[the recorded filename not restored at verification]
file internal/job/verification.go
--- anchor
	if v.Filename != "" {
--- replace
	if false {
--- end
