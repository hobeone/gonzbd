pkg ./internal/unpack/
run TestGoUnRAR_LinkMembersAreNeverCreated|TestExtractEntryRarengine_LinkMemberLeavesTheRootUntouched|TestPublishStagedExtraction_PublishesNoLinks|TestUnRAR_LinkMembersAreNotPublished|TestUnRAR_SkipSymlinksFlag

# Pins for "no links in a job": every extraction path skips link members.
# Each arm below re-enables one link type, or one listing or publishing
# decision, that the skip removed.

[symlink members are created]
file internal/unpack/go_unrar.go
--- anchor
		skipLinkEntry(fh, opts, log)
		return nil
--- replace
		if fh.LinkType == rarengine.LinkUnixSymlink {
			return root.Symlink(fh.LinkTarget, destRel)
		}
		skipLinkEntry(fh, opts, log)
		return nil
--- end

[hard-link members are created]
file internal/unpack/go_unrar.go
--- anchor
		skipLinkEntry(fh, opts, log)
		return nil
--- replace
		if fh.LinkType == rarengine.LinkHardLink {
			return root.Link(fh.LinkTarget, destRel)
		}
		skipLinkEntry(fh, opts, log)
		return nil
--- end

[file-reference members are copied]
file internal/unpack/go_unrar.go
--- anchor
		skipLinkEntry(fh, opts, log)
		return nil
--- replace
		if fh.LinkType == rarengine.LinkFileCopy {
			b, err := root.ReadFile(fh.LinkTarget)
			if err != nil {
				return err
			}
			return root.WriteFile(destRel, b, 0o600)
		}
		skipLinkEntry(fh, opts, log)
		return nil
--- end

[a link member is not reported]
file internal/unpack/rar_links.go
--- anchor
		opts.OnLine("Skipping link: " + fh.Name)
--- replace
		_ = fh.Name
--- end

[a skipped link member is listed as extracted]
file internal/unpack/rar_links.go
--- anchor
	return !fh.IsDir && fh.LinkType == rarengine.LinkNone
--- replace
	return !fh.IsDir
--- end

[the external extractors' symlinks are published]
file internal/unpack/snapshot.go
--- anchor
	if !d.Type().IsRegular() {
--- replace
	if false {
--- end

[unrar is never told to skip symlinks]
file internal/unpack/unrar.go
--- anchor
	if opts.UnrarVersion >= unrarSkipSymlinksVersion {
--- replace
	if false {
--- end

[an unrar too old for -ol- is passed it]
file internal/unpack/unrar.go
--- anchor
	if opts.UnrarVersion >= unrarSkipSymlinksVersion {
--- replace
	if true {
--- end

[every name of a hard-linked inode is published]
file internal/unpack/snapshot.go
--- anchor
	if _, dup := seen[id]; dup {
--- replace
	if _, dup := seen[id]; dup && false {
--- end
