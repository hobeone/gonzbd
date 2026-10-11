pkg ./internal/app/
run TestRenameJob_RefusedAfterAFailedFirstArticle

# #825, in process: a permanent failure registers the file and the assembler
# creates it, so the rename must be refused. Run alone, so the kill is this
# test's and not the restart test's (download_begun_owner.spec).
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
