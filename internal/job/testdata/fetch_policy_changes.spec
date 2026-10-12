pkg ./internal/job/
run TestTakeFetchPolicyChanges|TestInstallFileVerification_RestoreDoesNotUndoARelease

# setFileFetchPolicy is the one writer of a file's fetch policy (#858). It
# records each change for the recorder, and refuses to put a hold over a
# release.

[the writer records no change]
file internal/job/content.go
--- anchor
	if !f.fetchChanged {
		f.fetchChanged = true
--- replace
	if false {
		f.fetchChanged = true
--- end

[the take does not forget what it took]
file internal/job/content.go
--- anchor
		f.fetchChanged = false
--- replace
--- end

[a hold is restored over a release]
file internal/job/content.go
--- anchor
	if stored == FetchIfNeeded && p.par2Recovered {
--- replace
	if false {
--- end

[a restore the release overrode is not handed to the record]
file internal/job/content.go
--- anchor
	if f.Fetch == stored && stored == policy {
--- replace
	if f.Fetch == stored {
--- end
