pkg ./internal/unpack/
run TestMemberPath|TestExtractMember

# The per-member name check of the RAR extraction loops, and the go_unrar
# member step that applies it and reports each member's close verdict.

[MemberPath accepts every name]
file internal/unpack/sanitized_path.go
--- anchor
	if err != nil {
		log.Warn("skipping entry with bad path", "raw_name", name, "err", err)
--- replace
	if err != nil && false {
		log.Warn("skipping entry with bad path", "raw_name", name, "err", err)
--- end

[go_unrar's member step ignores the name check]
file internal/unpack/go_unrar.go
--- anchor
	sp, ok := MemberPath(fh.Name, opts, log)
	if !ok {
--- replace
	sp, ok := MemberPath(fh.Name, opts, log)
	if false && !ok {
--- end

[go_unrar drops the close verdict of a member skipped for its name]
file internal/unpack/go_unrar.go
--- anchor
		return "", "", CloseMember(entry)
--- replace
		_ = CloseMember(entry)
		return "", "", nil
--- end

[go_unrar drops the close verdict of a written member]
file internal/unpack/go_unrar.go
--- anchor
	if err := CloseMember(entry); err != nil {
		return "", "", err
	}
--- replace
	_ = CloseMember(entry)
--- end
