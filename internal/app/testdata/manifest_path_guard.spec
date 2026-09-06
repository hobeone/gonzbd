pkg ./internal/app/
run TestManifestPath_RejectsUnsafeJobID

[the path-safety guard neutered]
file internal/app/manifestpath.go
--- anchor
	if !jobIDIsPathSafe(jobID) {
--- replace
	if false {
--- end

[separator rejection neutered]
file internal/app/manifestpath.go
--- anchor
	if strings.ContainsAny(id, `/\`) {
--- replace
	if strings.ContainsAny(id, "") {
--- end
