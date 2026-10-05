pkg ./internal/app/
run TestRetryHistoryJob_NoJobCanTakeItsNameAfterTheRestore

# The window between the restore and the retry's own registration: a job that
# takes the name there must be refused; unrefused, the retry's Add then fails
# and its undo moves that job's early files back to _FAILED_.
[the retry moves the directory without claiming its name]
file internal/app/app.go
--- anchor
	if app.dispatcher != nil {
		releaseName, err := app.dispatcher.ReserveName(jobID, j.Name())
--- replace
	if false {
		releaseName, err := app.dispatcher.ReserveName(jobID, j.Name())
--- end
