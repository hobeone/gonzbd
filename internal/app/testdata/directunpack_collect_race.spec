pkg ./internal/app/
run ^TestHandOff_(LastFileCompletingAtTheCollect_LeavesNoUnpackerAwaitedWithoutAVolume|CompletionAfterTheCollect_StartsNoUnpacker)$

# A hand-off that does not wait for the download-finished report reads
# whether the download finished before anything can drop a volume's feed, and
# no DirectUnpacker is started for a job already handed to post-processing.

[the download-finished read follows the collect]
file internal/app/app.go
--- anchor
	if app.directUnpackCollectHook != nil {
		app.directUnpackCollectHook(j.ID())
	}
	du := app.duOrch.collect(j.ID())
--- replace
	du := app.duOrch.collect(j.ID())
	if app.directUnpackCollectHook != nil {
		app.directUnpackCollectHook(j.ID())
	}
	downloadFinished = j.IsComplete()
--- end

[the download-finished read follows forgetJob, just before the collect]
file internal/app/app.go
--- anchor
	if app.directUnpackCollectHook != nil {
		app.directUnpackCollectHook(j.ID())
	}
	du := app.duOrch.collect(j.ID())
--- replace
	if app.directUnpackCollectHook != nil {
		app.directUnpackCollectHook(j.ID())
	}
	downloadFinished = j.IsComplete()
	du := app.duOrch.collect(j.ID())
--- end

[maybeStart starts an unpacker for a job handed to post-processing]
file internal/app/directunpack_orchestrator.go
--- anchor
		if app.postProcAdmissions.has(j) || app.transitions.wasRemoved(j) {
--- replace
		if app.transitions.wasRemoved(j) {
--- end
