pkg ./internal/dispatch/
run TestSetName_WaitsForAHydrationInstallingFilenames$|TestLoadProgress_RecordsOnlyALoadItMade$

# LoadProgress goes through Hydrate even for a job with progress, so a rename
# waits for a hydration that has attached content but not yet installed its
# verified filenames (#838). It records a load only for a job it made
# resident.

[LoadProgress skips a job that has progress]
file internal/dispatch/registry.go
--- anchor
	wasResident := j.Resident()
--- replace
	if j.HasProgress() {
		return nil
	}
	wasResident := j.Resident()
--- end

[LoadProgress treats an evicted job with progress as resident]
file internal/dispatch/registry.go
--- anchor
	wasResident := j.Resident()
--- replace
	wasResident := j.HasProgress()
--- end

[LoadProgress records a load for a job already resident]
file internal/dispatch/registry.go
--- anchor
	if !wasResident {
		d.markResident(id)
--- replace
	if !wasResident || true {
		d.markResident(id)
--- end

[LoadProgress never records a load]
file internal/dispatch/registry.go
--- anchor
	if !wasResident {
		d.markResident(id)
--- replace
	if !wasResident && false {
		d.markResident(id)
--- end
