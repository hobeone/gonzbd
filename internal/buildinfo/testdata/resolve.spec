pkg ./internal/buildinfo/
run TestResolve

[the vcs.revision fallback for an unset commit neutered]
file internal/buildinfo/buildinfo.go
--- anchor
		info.Commit = shorten(revision)
--- replace
		info.Commit = ""
--- end

[the link-time commit no longer wins over vcs.revision]
file internal/buildinfo/buildinfo.go
--- anchor
	if info.Commit == "" {
		info.Commit = shorten(revision)
--- replace
	if true {
		info.Commit = shorten(revision)
--- end

[vcs settings for a different commit no longer ignored]
file internal/buildinfo/buildinfo.go
--- anchor
	} else if !strings.HasPrefix(revision, info.Commit) {
--- replace
	} else if false && !strings.HasPrefix(revision, info.Commit) {
--- end

[link-time commit time no longer wins over vcs.time]
file internal/buildinfo/buildinfo.go
--- anchor
	if info.CommitTime == "" {
--- replace
	if true {
--- end

[link-time dirty no longer wins over vcs.modified]
file internal/buildinfo/buildinfo.go
--- anchor
	if !dirtySet {
--- replace
	if true || !dirtySet {
--- end

[vcs.modified no longer read]
file internal/buildinfo/buildinfo.go
--- anchor
		info.Dirty = modified == "true"
--- replace
		info.Dirty = modified == "never"
--- end

[vcs.time no longer read]
file internal/buildinfo/buildinfo.go
--- anchor
		info.CommitTime = vcsTime
--- replace
		info.CommitTime = vcsTime + "x"
--- end
