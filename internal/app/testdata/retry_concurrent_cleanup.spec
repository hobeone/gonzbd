pkg ./internal/app/
run TestRetryHistoryJob_LosingConcurrentRetryKeepsTheWinnersRows|TestRetryHistoryJob_Failed(Add|Flush)RemovesTheQueueManifest

[a losing concurrent retry reclaims the winner's rows]
file internal/app/app.go
--- anchor
			if _, held := app.dispatcher.Job(jobID); held {
--- replace
			if _, held := app.dispatcher.Job(jobID); held && false {
--- end

[the guard skips every abandoned retry's cleanup]
file internal/app/app.go
--- anchor
			if _, held := app.dispatcher.Job(jobID); held {
--- replace
			if _, held := app.dispatcher.Job(jobID); held || true {
--- end
