pkg ./internal/par2/
run ^(TestCollectManifests_SkipsASetIDAlreadyParsed|TestIdentification_SliceSize)$

[a set parsed twice under two names is collected twice]
file internal/par2/fsops.go
--- anchor
			return i.id == parsed.SetID && i.sliceSize == parsed.SliceSize
--- replace
			return false
--- end

[a second set is skipped whatever its SetID]
file internal/par2/fsops.go
--- anchor
			return i.id == parsed.SetID && i.sliceSize == parsed.SliceSize
--- replace
			return true
--- end

[a SetID repeated with another slice size is skipped]
file internal/par2/fsops.go
--- anchor
			return i.id == parsed.SetID && i.sliceSize == parsed.SliceSize
--- replace
			return i.id == parsed.SetID
--- end
