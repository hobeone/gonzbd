pkg ./internal/downloader/
run TestShouldDeactivateOptional|TestClearDeactivation|TestApplyPenalty_NonOptionalNotDeactivated

[the non-optional rule dropped from its owner]
file internal/downloader/penalty.go
--- anchor
	if !s.cfg.Optional {
--- replace
	if false {
--- end

[ApplyPenalty no longer consults the owner]
file internal/downloader/server.go
--- anchor
	if shouldDeactivateOptional(s) {
--- replace
	if false {
--- end

[ApplyPenalty deactivates every penalized server]
file internal/downloader/server.go
--- anchor
	if shouldDeactivateOptional(s) {
--- replace
	if true {
--- end
