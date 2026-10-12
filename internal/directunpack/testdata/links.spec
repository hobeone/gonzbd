pkg ./internal/directunpack/
run TestDirectUnpack_ContainmentViolationFailsTheRun|TestDirectUnpack_LinkMembersAreNeverCreated|TestDirectUnpack_HardLinkMemberToAJobFileIsSkipped|TestDirectUnpacker_ExtractMember

[DirectUnpack accepts results without the containment gate]
file internal/directunpack/directunpack.go
--- anchor
	if cErr := fsutil.CheckContainment(d.extractDir); cErr != nil {
--- replace
	if cErr := fsutil.CheckContainment(d.extractDir); cErr != nil && false {
--- end

# DirectUnpack extracts into a staging directory inside the job directory;
# these re-enable link creation in the extractor it shares with
# post-processing.

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
