pkg ./internal/app/
run TestSetDownloadDir_

[the unsettled-row check neutered]
file internal/app/app.go
--- anchor
			if !row.View.Outcome.IsSettled() {
--- replace
			if !row.View.Outcome.IsSettled() && false {
--- end

[the same-value shortcut dropped, so a no-op is refused with a job queued]
file internal/app/app.go
--- anchor
	if filepath.Clean(dir) == filepath.Clean(app.downloadDir()) {
--- replace
	if false {
--- end

[the same-value comparison made on uncleaned strings]
file internal/app/app.go
--- anchor
	if filepath.Clean(dir) == filepath.Clean(app.downloadDir()) {
--- replace
	if dir == app.downloadDir() {
--- end

[a settled row counted as unfinished]
file internal/app/app.go
--- anchor
			if !row.View.Outcome.IsSettled() {
--- replace
			if row.View.Outcome.IsSettled() || true {
--- end
