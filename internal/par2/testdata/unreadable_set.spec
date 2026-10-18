pkg ./internal/par2/
run ^TestIdentify_ReportsUnreadableSets$

# A par2 set on disk that contributes no entry is reported as Unreadable
# (#1000), whichever way it fails to contribute, and on both of Identify's
# returns.

[a set whose parse fails is not reported]
file internal/par2/fsops.go
--- anchor
				"file", filepath.Base(parFile), "err", err)
			c.unreadable = append(c.unreadable, set.Name)
--- replace
				"file", filepath.Base(parFile), "err", err)
--- end

[a set that parses to no entry is not reported]
file internal/par2/fsops.go
--- anchor
		if len(parsed.Files) == 0 && len(parsed.RecoveryFileIDs) == 0 {
--- replace
		if false {
--- end

[an empty set is reported but still counted as a parsed set]
file internal/par2/fsops.go
--- anchor
				"file", filepath.Base(parFile))
			c.unreadable = append(c.unreadable, set.Name)
			continue
--- replace
				"file", filepath.Base(parFile))
			c.unreadable = append(c.unreadable, set.Name)
--- end

[Identify drops the report when no set describes a file]
file internal/par2/identify.go
--- anchor
		return Identification{Unaccounted: c.undescribed, Unreadable: c.unreadable, sets: infos}, nil
--- replace
		return Identification{Unaccounted: c.undescribed, sets: infos}, nil
--- end

[Identify drops the report beside a readable set]
file internal/par2/identify.go
--- anchor
	id := Identification{Ignored: ignored, Unreadable: c.unreadable, sets: infos}
--- replace
	id := Identification{Ignored: ignored, sets: infos}
--- end
