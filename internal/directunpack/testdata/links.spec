pkg ./internal/directunpack/
run TestDirectUnpack_ContainmentViolationFailsTheRun|TestDirectUnpack_SymlinkMemberSkippedByDefault|TestDirectUnpack_SymlinkMember|TestDirectUnpack_EscapingSymlinkRefusedSetContinues

[DirectUnpack accepts results without the containment gate]
file internal/directunpack/directunpack.go
--- anchor
	if cErr := fsutil.CheckContainment(d.extractDir); cErr != nil {
--- replace
	if cErr := fsutil.CheckContainment(d.extractDir); cErr != nil && false {
--- end

[DirectUnpack ignores extract_symlinks]
file internal/directunpack/directunpack.go
--- anchor
			ExtractSymlinks:  d.opts.ExtractSymlinks,
--- replace
			ExtractSymlinks:  true,
--- end
