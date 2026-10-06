pkg ./internal/unpack/
run TestSymlinkBatch_LinkUnderASymlinkedParentIsRefused|TestSymlinkBatch_ReplacedLinkIsCheckedOnce|TestSymlinkBatch_RevalidatesUntilStable|TestValidateLinkTarget|TestCreateHardLinkEntry_TargetNotARegularFile|TestRecordSymlinkEntry|TestPrepareLinkDest|TestVerifyCreatedLink|TestRefuseSymlinkedParent|TestSymlinkBatch_FinalCheckIsLexicallyConservative|TestSymlinkBatch_LaterLinkMakesEarlierLinkEscape|TestSymlinkBatch_FileReplacedBySymlinkMakesEarlierLinkEscape|TestSymlinkBatch_NothingCreatedBeforeFinish|TestGoUnRAR_SymlinkMember|TestGoUnRAR_SolidArchiveWithLink|TestGoUnRAR_EscapingSymlinkRefused|TestGoUnRAR_SymlinkMembersSkippedByDefault|TestSymlinkMember_OneFolderRefusesTargetsOutsideFlatLayout|TestLinkMembers_NameTooLongIsARefusalNotAFailure|TestIsLinkUnsupported|TestPhysicalResolve_OnDiskTargetsKeepBackslashes|TestResolveSymlinkTarget|TestResolveSymlinkTarget_Loop|TestExtractEntryRarengine_SymlinkOverwrite

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

# Review of #741: Finish only creates links whose recorded path stays the
# link it made, and re-checks until the set is stable.

[a link is created through a symlinked parent]
file internal/unpack/rar_links.go
--- anchor
		err := refuseSymlinkedParent(root, p.destRel)
--- replace
		var err error
--- end

[a symlinked parent component is not refused]
file internal/unpack/rar_links.go
--- anchor
		if fi.Mode()&fs.ModeSymlink != 0 {
			return refusef("parent %q of %q is a symlink", p, destRel)
--- replace
		if fi.Mode()&fs.ModeSymlink != 0 && false {
			return refusef("parent %q of %q is a symlink", p, destRel)
--- end

[a link replaced by a same-named member stays listed twice]
file internal/unpack/rar_links.go
--- anchor
			created = slices.DeleteFunc(created, func(r string) bool { return r == p.destRel })
--- replace
			created = slices.DeleteFunc(created, func(r string) bool { return false })
--- end

[the final check stops after one pass]
file internal/unpack/rar_links.go
--- anchor
				changed = true
--- replace
				changed = false
--- end

[flat unpack keeps a "." target]
file internal/unpack/rar_links.go
--- anchor
		if c := path.Clean(target); c != path.Base(c) || c == ".." || c == "." {
--- replace
		if c := path.Clean(target); c != path.Base(c) || c == ".." {
--- end

[a hard link or copy of a directory or symlink is attempted]
file internal/unpack/rar_links.go
--- anchor
	if !fi.Mode().IsRegular() {
--- replace
	if false {
--- end

[a drive letter is refused in a target read back from disk]
file internal/unpack/rar_links.go
--- anchor
	case archive && len(t) >= 2 && t[1] == ':' && (t[0]|0x20 >= 'a' && t[0]|0x20 <= 'z'):
--- replace
	case len(t) >= 2 && t[1] == ':' && (t[0]|0x20 >= 'a' && t[0]|0x20 <= 'z'):
--- end

[a drive letter in an archive target is accepted]
file internal/unpack/rar_links.go
--- anchor
	case archive && len(t) >= 2 && t[1] == ':' && (t[0]|0x20 >= 'a' && t[0]|0x20 <= 'z'):
--- replace
	case false && archive:
--- end

[recordSymlinkEntry queues a link that escapes at record time]
file internal/unpack/rar_links.go
--- anchor
	if err := checkLinkAgainstDisk(root, destRel, target); err != nil {
		return err
	}
	opts.Symlinks.pending = append(
--- replace
	if err := checkLinkAgainstDisk(root, destRel, target); err != nil && false {
		return err
	}
	opts.Symlinks.pending = append(
--- end

[prepareLinkDest removes a directory in the way]
file internal/unpack/rar_links.go
--- anchor
	if fi.IsDir() {
--- replace
	if fi.IsDir() && false {
--- end

[the final check does not validate the target read back]
file internal/unpack/rar_links.go
--- anchor
	if err := validateLinkTarget(target, false); err != nil {
		return err
	}
	if err := checkLinkAgainstDisk(root, destRel, target); err != nil {
--- replace
	if err := validateLinkTarget(target, false); err != nil && false {
		return err
	}
	if err := checkLinkAgainstDisk(root, destRel, target); err != nil {
--- end
