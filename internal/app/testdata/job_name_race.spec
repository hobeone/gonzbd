pkg ./internal/app/
run ^(TestAddJob_ConcurrentCallsUnderOneNameGetDistinctNames|TestAddJob_ANameTakenAfterItWasChosenIsChosenAgain|TestRenameJob_ANameTakenAfterItWasChosenIsChosenAgain)$

[a refused name is not chosen again]
file internal/app/rename.go
--- anchor
		if !errors.Is(err, dispatch.ErrJobNameTaken) {
--- replace
		if true {
--- end

[registration does not check the name]
file internal/dispatch/registry.go
--- anchor
	if err := d.admitNameLocked(j.ID(), h.Name); err != nil {
--- replace
	if err := d.admitNameLocked(j.ID(), h.Name); false && err != nil {
--- end
