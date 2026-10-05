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
	if otherID, taken := d.nameHolderLocked(j.ID(), h.Name); taken {
--- replace
	if otherID, taken := d.nameHolderLocked(j.ID(), h.Name); false && taken {
--- end
