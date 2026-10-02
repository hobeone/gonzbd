pkg ./internal/api/
run TestBuildMetadataOnTheWire

[mode=about drops the commit time]
file internal/api/about.go
--- anchor
		"commit_time":  s.commitTime,
--- replace
		"commit_time":  "",
--- end

[mode=status build_info drops the dirty flag]
file internal/api/statusbuildinfo.go
--- anchor
		"dirty":       s.dirty,
--- replace
		"dirty":       false,
--- end
