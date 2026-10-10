pkg ./internal/directunpack/
run TestDirectUnpack_ContainmentViolationFailsTheRun|TestDirectUnpack_LinkMembersAreNeverCreated|TestDirectUnpack_HardLinkMemberToAJobFileIsSkipped

[DirectUnpack accepts results without the containment gate]
file internal/directunpack/directunpack.go
--- anchor
	if cErr := fsutil.CheckContainment(d.extractDir); cErr != nil {
--- replace
	if cErr := fsutil.CheckContainment(d.extractDir); cErr != nil && false {
--- end

# DirectUnpack extracts into the job directory; these re-enable link creation
# in the extractor it shares with post-processing.

[DirectUnpack creates hard-link members over job files]
file internal/unpack/go_unrar.go
--- anchor
		skipLinkEntry(fh, opts, log)
		return nil
--- replace
		if fh.LinkType == rarengine.LinkHardLink {
			_ = root.Remove(destRel)
			return root.Link(fh.LinkTarget, destRel)
		}
		skipLinkEntry(fh, opts, log)
		return nil
--- end

[DirectUnpack creates symlink members]
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

[DirectUnpack copies file-reference members]
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

[DirectUnpack lists skipped link members]
file internal/unpack/rar_links.go
--- anchor
	return !fh.IsDir && fh.LinkType == rarengine.LinkNone
--- replace
	return !fh.IsDir
--- end
