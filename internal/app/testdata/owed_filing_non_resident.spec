pkg ./internal/app/
run ^TestFileOwedUnwantedFailures_CompleteJobWithUnreadableManifestIsFiled$
timeout 5m

# A job owed an unwanted-extension filing whose hydration failed is not
# resident and has no Assessing worker, so the filing is made at once
# (fileOwedUnwantedFailure), not deferred behind a seeded Complete flag.

[the filing is deferred to Assessing whether or not the job is resident]
file internal/app/unwanted.go
--- anchor
	app.enqueuePostProc(j, row.Header, failMsg, j.Resident())
--- replace
	app.enqueuePostProc(j, row.Header, failMsg, true)
--- end
