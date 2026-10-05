pkg ./internal/unpack/
run TestSymlinkBatch_FinalCheckIsLexicallyConservative|TestSymlinkBatch_LaterLinkMakesEarlierLinkEscape|TestSymlinkBatch_FileReplacedBySymlinkMakesEarlierLinkEscape|TestSymlinkBatch_NothingCreatedBeforeFinish|TestGoUnRAR_SymlinkMember|TestGoUnRAR_SolidArchiveWithLink|TestGoUnRAR_EscapingSymlinkRefused|TestGoUnRAR_SymlinkMembersSkippedByDefault|TestSymlinkMember_OneFolderRefusesTargetsOutsideFlatLayout|TestLinkMembers_NameTooLongIsARefusalNotAFailure|TestIsLinkUnsupported|TestPhysicalResolve_OnDiskTargetsKeepBackslashes|TestResolveSymlinkTarget|TestResolveSymlinkTarget_Loop|TestExtractEntryRarengine_SymlinkOverwrite

# Pins for the symlink policy of #740: skipped by default, created only after
# the last member, re-validated against the final tree.

[symlinks are extracted although extract_symlinks is off]
file internal/unpack/rar_links.go
--- anchor
	if !opts.ExtractSymlinks {
--- replace
	if false {
--- end

[symlinks are created in place instead of after the last member]
file internal/unpack/rar_links.go
--- anchor
	opts.Symlinks.pending = append(opts.Symlinks.pending, pendingSymlink{destRel: destRel, destPath: destPath, name: fh.Name, target: fh.LinkTarget, fh: fh})
	return nil
--- replace
	_ = root.Symlink(fh.LinkTarget, destRel)
	return nil
--- end

[the final re-validation of created links is skipped]
file internal/unpack/rar_links.go
--- anchor
			if err := verifyCreatedLink(root, rel); err != nil {
--- replace
			if err := verifyCreatedLink(root, rel); err != nil && false {
--- end

[the physical resolution of a target is not enforced]
file internal/unpack/rar_links.go
--- anchor
	_, err = physicalResolve(root, parent+"/"+target)
	return err
--- replace
	_, _ = physicalResolve(root, parent+"/"+target)
	return nil
--- end

[flat unpack keeps targets that leave the flattened layout]
file internal/unpack/rar_links.go
--- anchor
		if c := path.Clean(target); c != path.Base(c) || c == ".." || c == "." {
--- replace
		if c := path.Clean(target); false && c != path.Base(c) {
--- end

[a link the destination cannot hold fails the set]
file internal/unpack/rar_links.go
--- anchor
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENAMETOOLONG)
--- replace
	return false
--- end

[on-disk symlink targets get their backslashes rewritten]
file internal/unpack/rar_links.go
--- anchor
			queue = append(strings.Split(dest, "/"), queue...)
--- replace
			queue = append(strings.Split(strings.ReplaceAll(dest, "\\", "/"), "/"), queue...)
--- end

[the lexical join of the real parent and the target is not checked]
file internal/unpack/rar_links.go
--- anchor
	if !fsutil.PathWithin(realRoot, filepath.Join(realParent, filepath.FromSlash(target))) {
--- replace
	if false && !fsutil.PathWithin(realRoot, filepath.Join(realParent, filepath.FromSlash(target))) {
--- end
