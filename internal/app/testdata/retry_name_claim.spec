pkg ./internal/app/
run TestRetryHistoryJob_NoJobCanTakeItsNameAfterTheRestore|TestRetryHistoryJob_AbortedRetryReturnsItsDirectoryAndItsName|TestRetryHistoryJob_RefusesANameAQueuedJobHasBeforeMovingAnything

# The retry claims its name in the registry before it moves the directory.
[the retry moves the directory without claiming its name]
file internal/app/app.go
--- anchor
	releaseName, err := app.dispatcher.ReserveName(jobID, j.Name())
--- replace
	releaseName, err := func() {}, error(nil)
--- end

# The claim lives only as long as the retry.
[the retry never releases its name]
file internal/app/app.go
--- anchor
	defer releaseName()
--- replace
	_ = releaseName
--- end
