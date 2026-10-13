pkg ./internal/app/
run TestLooseRecord_ResumedCompletionSurvivesEviction|TestLooseRecord_RetryVerifiesWrittenArticles|TestVerifyRetry_ReadsEveryFileAndReportsWhatItFinished

# A file the verifier finishes by path is marked complete by the hydration
# itself, so its Resumed completion skips the residency check before the mark
# and survives an eviction before it is consumed.

[the hydration leaves the finished file for the consumer to mark]
file internal/app/residency.go
--- anchor
			if err := j.MarkFileComplete(fi); err != nil {
--- replace
			if err := error(nil); err != nil {
--- end

[the consumer runs a resumed completion through the residency check and the mark]
file internal/app/app.go
--- anchor
	if !fc.Resumed {
		if !j.Resident() {
--- replace
	if true {
		if !j.Resident() {
--- end
