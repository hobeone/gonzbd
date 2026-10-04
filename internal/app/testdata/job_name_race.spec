pkg ./internal/app/
run ^(TestAddJob_ConcurrentCallsUnderOneNameGetDistinctNames|TestAddJob_ANameTakenAfterItWasChosenIsChosenAgain|TestRenameJob_ANameTakenAfterItWasChosenIsChosenAgain|TestRetryHistoryJob_RefusesWhenAnotherJobTookItsName)$

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

[a retry's lost name is reported as a plain failure]
file internal/app/app.go
--- anchor
		if errors.Is(err, dispatch.ErrJobNameTaken) {
			// Not chosen again, as AddJob does: the name is the directory
--- replace
		if false {
			// Not chosen again, as AddJob does: the name is the directory
--- end
