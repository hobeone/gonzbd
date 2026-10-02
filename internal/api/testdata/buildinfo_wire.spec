pkg ./internal/api/
run TestBuildMetadataOnTheWire

[the shared helper drops the commit time]
file internal/api/statusbuildinfo.go
--- anchor
	m["commit_time"] = s.build.CommitTime
--- replace
	m["commit_time"] = ""
--- end

[the shared helper drops the dirty flag]
file internal/api/statusbuildinfo.go
--- anchor
	m["dirty"] = s.build.Dirty
--- replace
	m["dirty"] = false
--- end

[status_overview stops writing the build fields]
file internal/api/statusoverview.go
--- anchor
	s.writeBuildFields(general)
--- replace
	_ = general
--- end

[mode=about stops writing the build fields]
file internal/api/about.go
--- anchor
	s.writeBuildFields(about)
--- replace
	_ = about
--- end

[status build_info stops writing the build fields]
file internal/api/statusbuildinfo.go
--- anchor
	s.writeBuildFields(resp)
--- replace
	_ = resp
--- end
