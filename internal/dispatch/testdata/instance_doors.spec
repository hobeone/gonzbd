pkg ./internal/dispatch/
run TestRemoveJob_LeavesALaterInstanceAlone|TestOccupyJob_LeavesALaterInstanceAlone

# The instance checks of RemoveJob and OccupyJob, each neutered on its own.

[RemoveJob ignores the expected instance]
file internal/dispatch/registry.go
--- anchor
	if e == nil || (expected != nil && e.j != expected) {
--- replace
	if e == nil {
--- end

[OccupyJob ignores the expected instance]
file internal/dispatch/occupy.go
--- anchor
	if !d.admitsLocked(id) || (expected != nil && d.byID[id].j != expected) {
--- replace
	if !d.admitsLocked(id) {
--- end
