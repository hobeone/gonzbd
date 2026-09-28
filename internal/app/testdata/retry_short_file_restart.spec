pkg ./internal/app/
run TestRetryHistoryJob_RefetchesTheArticleAShortFileMissed/after_a_restart

# The stale Complete comes from the history database, not from anything the
# first attempt left in memory, so a retry after a restart needs the same
# reset and dies to the same mutation.
[ResetForRetry clears Complete only for files whose failed articles it reset]
file internal/job/content.go
--- anchor
				j.progress.failed.Clear(i)
			}
			if !j.progress.done.Get(i) {
--- replace
				j.progress.failed.Clear(i)
				unresolved = true
			}
			if false {
--- end
