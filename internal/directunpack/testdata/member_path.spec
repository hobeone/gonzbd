pkg ./internal/directunpack/
run TestDirectUnpacker_ExtractMember

# DirectUnpack's member step: it applies unpack.MemberPath and reports each
# member's close verdict.

[DirectUnpack ignores the name check]
file internal/directunpack/directunpack.go
--- anchor
	sp, ok := unpack.MemberPath(fh.Name, opts, d.log)
	if !ok {
--- replace
	sp, ok := unpack.MemberPath(fh.Name, opts, d.log)
	if false && !ok {
--- end

[DirectUnpack drops the close verdict of a member skipped for its name]
file internal/directunpack/directunpack.go
--- anchor
		if err := unpack.CloseMember(entry); err != nil {
			return "", fmt.Errorf("directunpack: verify skipped %s: %w", fh.Name, err)
--- replace
		if err := unpack.CloseMember(entry); err != nil && false {
			return "", fmt.Errorf("directunpack: verify skipped %s: %w", fh.Name, err)
--- end

[DirectUnpack drops the close verdict of a written member]
file internal/directunpack/directunpack.go
--- anchor
	if err := unpack.CloseMember(entry); err != nil {
		return "", fmt.Errorf("directunpack: verify %s: %w", fh.Name, err)
--- replace
	if err := unpack.CloseMember(entry); err != nil && false {
		return "", fmt.Errorf("directunpack: verify %s: %w", fh.Name, err)
--- end
