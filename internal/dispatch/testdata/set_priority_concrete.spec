pkg ./internal/dispatch/
run TestSetPriority_Validation

[SetPriority admits any defined sentinel]
file internal/dispatch/registry.go
--- anchor
	if priority < -128 || priority > 127 || !constants.Priority(int8(priority)).IsConcrete() { //nolint:gosec // G115: priority fits in int8
--- replace
	if priority < -128 || priority > 127 || (!constants.Priority(int8(priority)).IsConcrete() && priority != -100 && priority != -2 && priority != -4) { //nolint:gosec // G115: priority fits in int8
--- end

[SetPriority admits every int8]
file internal/dispatch/registry.go
--- anchor
	if priority < -128 || priority > 127 || !constants.Priority(int8(priority)).IsConcrete() { //nolint:gosec // G115: priority fits in int8
--- replace
	if priority < -128 || priority > 127 || (false && !constants.Priority(int8(priority)).IsConcrete()) { //nolint:gosec // G115: priority fits in int8
--- end
