pkg ./internal/app/
run TestRetryHistoryJob_RefetchesTheArticleAShortFileMissed|TestRetryHistoryJob_ReattemptsAnArticleStillMissing

# The end-to-end half of internal/job/testdata/retry_short_file.spec: with
# ResetForRetry back to clearing Complete only for files whose failed articles
# it reset, a retry of a job whose file finalized short never re-fetches the
# missing article.
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
