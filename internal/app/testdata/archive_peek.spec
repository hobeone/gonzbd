# Red check for the archive peek (internal/app/archive_peek.go): each mutation
# neuters one decision and a named test must die.
#
#     go run ./scripts/mutate internal/app/testdata/archive_peek.spec
pkg ./internal/app/
run TestPeek_|TestArchivePeek_|TestBlockForUnwanted_
timeout 10m

[a file with a failed article is read anyway]
file internal/app/archive_peek.go
--- anchor
	if p == nil || hasFailedArticle(m, p, fc.FileIdx) {
--- replace
	if p == nil {
--- end

[the off action does not stop the peek]
file internal/app/archive_peek.go
--- anchor
	if rules.Action() == unwanted.ActionOff {
--- replace
	if false {
--- end

[a par2 file is never identified]
file internal/app/archive_peek.go
--- anchor
	if !isPar2 {
--- replace
	if !isPar2 || true {
--- end

[a RAR is never identified by its magic]
file internal/app/archive_peek.go
--- anchor
	if isRAR {
--- replace
	if isRAR && false {
--- end

[the pause action does not pause]
file internal/app/archive_peek.go
--- anchor
	moved, _, err := app.dispatcher.BlockUnwanted(jobID, action == unwanted.ActionPause)
--- replace
	moved, _, err := app.dispatcher.BlockUnwanted(jobID, false)
--- end

[the fail action does not file the job]
file internal/app/archive_peek.go
--- anchor
	if action == unwanted.ActionFail {
--- replace
	if false {
--- end

[a call that lost the race to block still acts]
file internal/app/archive_peek.go
--- anchor
	if !moved {
--- replace
	if false {
--- end

[a hit leaves the running unpacker alive]
file internal/app/archive_peek.go
--- anchor
	app.duOrch.abortJob(jobID)
--- replace
	_ = jobID
--- end

[the unpacker feed does not refuse a blocked job]
file internal/app/directunpack_orchestrator.go
--- anchor
	if st, _ := app.dispatcher.UnwantedState(fc.JobID); st == unwanted.StateBlocked {
--- replace
	if st, _ := app.dispatcher.UnwantedState(fc.JobID); st == unwanted.StateBlocked && false {
--- end
