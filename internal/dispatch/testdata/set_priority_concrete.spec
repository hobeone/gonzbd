pkg ./internal/dispatch/
run TestSetPriority_Validation|TestAdd_RefusesANonConcretePriority

[checkPriority admits any defined sentinel]
file internal/dispatch/registry.go
--- anchor
	if !p.IsConcrete() {
		return fmt.Errorf("%d is not a concrete priority", p)
--- replace
	if !p.IsConcrete() && p != -100 && p != -2 && p != -4 {
		return fmt.Errorf("%d is not a concrete priority", p)
--- end

[checkPriority admits every int8]
file internal/dispatch/registry.go
--- anchor
	if !p.IsConcrete() {
		return fmt.Errorf("%d is not a concrete priority", p)
--- replace
	if false {
		return fmt.Errorf("%d is not a concrete priority", p)
--- end

[register skips checkPriority]
file internal/dispatch/registry.go
--- anchor
	if err := checkPriority(h.Priority); err != nil {
--- replace
	if err := checkPriority(h.Priority); err != nil && false {
--- end

[SetPriority skips checkPriority]
file internal/dispatch/registry.go
--- anchor
	if err := checkPriority(priority); err != nil {
--- replace
	if err := checkPriority(priority); err != nil && false {
--- end
