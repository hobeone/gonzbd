pkg ./internal/dispatch/
run TestSetPriority_Validation

[SetPriority admits any defined sentinel]
file internal/dispatch/registry.go
--- anchor
	if !priority.IsConcrete() {
--- replace
	if !priority.IsConcrete() && priority != -100 && priority != -2 && priority != -4 {
--- end

[SetPriority admits every int8]
file internal/dispatch/registry.go
--- anchor
	if !priority.IsConcrete() {
--- replace
	if false {
--- end
