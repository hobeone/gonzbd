pkg ./internal/app/
run TestWriteNZBBackup_|TestAddJob_AFailedIngestDoesNotRemoveAnotherIngestsBackup

# A backup is created by exactly one call: the name is claimed by a link that
# fails rather than replaces.
[the claim replaces whatever holds the name]
file internal/app/app.go
--- anchor
		err := os.Link(staged, filepath.Join(nzbDir, name+".gz"))
--- replace
		err := os.Rename(staged, filepath.Join(nzbDir, name+".gz"))
--- end

# A refused name is the signal to choose again; any other error is a failure.
[a lost race is reported as a failure instead of choosing again]
file internal/app/app.go
--- anchor
		if !errors.Is(err, fs.ErrExist) {
--- replace
		if !errors.Is(err, fs.ErrExist) || true {
--- end

# The staging name must not outlive the call.
[a link failure that is not an existing name is retried as one]
file internal/app/app.go
--- anchor
		if !errors.Is(err, fs.ErrExist) {
--- replace
		if !errors.Is(err, fs.ErrExist) && false {
--- end

[the staging file is left in admin/nzb]
file internal/app/app.go
--- anchor
	defer func() { _ = os.Remove(staged) }()
--- replace
	defer func() { _ = staged }()
--- end
