pkg ./internal/buildinfo/
run TestResolve

[the vcs.revision fallback for an unset commit neutered]
file internal/buildinfo/buildinfo.go
--- anchor
		info.Commit = shorten(revision)
--- replace
		info.Commit = Unknown
--- end

[the link-time commit no longer wins over vcs.revision]
file internal/buildinfo/buildinfo.go
--- anchor
	if info.Commit == Unknown {
--- replace
	if true {
--- end

[vcs settings for a different commit no longer ignored]
file internal/buildinfo/buildinfo.go
--- anchor
	} else if !strings.HasPrefix(revision, info.Commit) {
--- replace
	} else if false && !strings.HasPrefix(revision, info.Commit) {
--- end

[vcs.modified no longer read]
file internal/buildinfo/buildinfo.go
--- anchor
	info.Dirty = modified == "true"
--- replace
	info.Dirty = modified == "never"
--- end
