pkg ./internal/directunpack/
run TestDirectUnpacker_ExtractMember

# DirectUnpack's member step is unpack.ExtractMember, called with this
# unpacker's options. These mutate the shared step and DirectUnpack's call into
# it, and require DirectUnpack's own test to notice: the name check, both close
# verdicts, and the flat-collision rename (#860, where DirectUnpack's former copy
# lacked the rename and skipped the second member).

[DirectUnpack ignores the name check]
file internal/unpack/go_unrar.go
--- anchor
	sp, ok := memberPath(fh.Name, opts, log)
	if !ok {
--- replace
	sp, ok := memberPath(fh.Name, opts, log)
	if false && !ok {
--- end

[DirectUnpack drops the close verdict of a member skipped for its name]
file internal/unpack/go_unrar.go
--- anchor
		return "", "", closeMember(entry)
--- replace
		_ = closeMember(entry)
		return "", "", nil
--- end

[DirectUnpack drops the close verdict of a written member]
file internal/unpack/go_unrar.go
--- anchor
	if err := closeMember(entry); err != nil {
		return "", "", err
	}
--- replace
	_ = closeMember(entry)
--- end

[DirectUnpack drops the member step's error]
file internal/directunpack/directunpack.go
--- anchor
	_, abs, err := unpack.ExtractMember(ctx, root, d.extractDir, fh, entry, opts, d.log)
	if err != nil {
--- replace
	_, abs, err := unpack.ExtractMember(ctx, root, d.extractDir, fh, entry, opts, d.log)
	if err != nil && false {
--- end

[a flat collision is skipped instead of renamed]
file internal/unpack/go_unrar.go
--- anchor
	if opts.OneFolder && !opts.OverwriteFiles {
		rel = uniquePath(root, rel)
--- replace
	if false {
		rel = uniquePath(root, rel)
--- end

[DirectUnpack does not pass flat_unpack to the member step]
file internal/directunpack/directunpack.go
--- anchor
		OneFolder:        d.opts.OneFolder,
--- replace
		OneFolder:        false,
--- end

[DirectUnpack does not pass overwrite_files to the member step]
file internal/directunpack/directunpack.go
--- anchor
		OverwriteFiles:   d.opts.OverwriteFiles,
--- replace
		OverwriteFiles:   true,
--- end
