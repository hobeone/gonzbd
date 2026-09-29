pkg ./internal/dispatch/
run TestDispatcher_AddPostAnomaly_AppendsRatherThanOverwrites|TestAppendPostAnomaly

[the append reverted to an overwrite]
file internal/dispatch/registry.go
--- anchor
	e.h.PostAnomaly = appendPostAnomaly(e.h.PostAnomaly, reason)
--- replace
	e.h.PostAnomaly = reason
--- end

[the "; " boundary dropped from the suffix check]
file internal/dispatch/registry.go
--- anchor
	if strings.HasSuffix(existing, "; "+next) {
--- replace
	if strings.HasSuffix(existing, next) {
--- end
