pkg ./internal/app/
run TestRenameJob_RefusesAJobWhoseDownloadHasStarted|TestRenameJob_GivesASafeUniqueName

[a same-name rename skips SetName, so a started job is not refused]
file internal/app/rename.go
--- anchor
	if name == row.Header.Name {
--- replace
	if name == row.Header.Name {
		return name, nil
	}
	if false {
--- end

[a same-name rename of an unstarted job changes the name]
file internal/app/rename.go
--- anchor
		if err := app.dispatcher.SetName(id, name); err != nil {
--- replace
		if err := app.dispatcher.SetName(id, name+"x"); err != nil {
--- end
