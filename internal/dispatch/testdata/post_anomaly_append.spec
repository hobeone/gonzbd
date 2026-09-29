pkg ./internal/dispatch/
run TestDispatcher_AddPostAnomaly_AppendsRatherThanOverwrites

[the append reverted to an overwrite]
file internal/dispatch/registry.go
--- anchor
	e.h.PostAnomaly = appendPostAnomaly(e.h.PostAnomaly, reason)
--- replace
	e.h.PostAnomaly = reason
--- end
