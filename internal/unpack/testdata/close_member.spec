pkg ./internal/unpack/
run TestCloseMember

# The defect this pins is the one the migration to rarengine's Reader/Entry API
# left behind: both refusal branches wrote `_ = entry.Close()`, which is what
# the pre-migration code did when the same line was an io.Copy drain that had
# no verdict to lose. Entry.Close does have one, so the discard silently threw
# away ErrNoNextVolume and ErrTruncatedFile.

# The whole guard neutered: every verdict is swallowed, which is the shape both
# call sites had before this change.
[no verdict reaches the caller]
file internal/unpack/go_unrar.go
--- anchor
	if err := entry.Close(); err != nil && !errors.Is(err, rarengine.ErrChecksumUnsupported) {
		return err
	}
	return nil
--- replace
	_ = entry.Close()
	return nil
--- end

# The filter widened from one sentinel to "anything non-nil is benign". A test
# that only checked the ErrChecksumUnsupported case would survive this.
#
# All four anchors carry the `return err` / `return nil` tail. That is no longer
# strictly required -- CloseMember now owns the check, so the bare `if` line
# appears once in the file and a short anchor would resolve. It was required
# when this spec was written, because the two extraction paths still inlined the
# same condition and the short form matched three sites; the tool refused it,
# which is the anchor-uniqueness failure AGENTS.md names. The tail is kept so a
# future call site that inlines the condition again cannot silently make these
# mutations ambiguous.
[the filter accepts every verdict as benign]
file internal/unpack/go_unrar.go
--- anchor
	if err := entry.Close(); err != nil && !errors.Is(err, rarengine.ErrChecksumUnsupported) {
		return err
	}
	return nil
--- replace
	if err := entry.Close(); err != nil && false {
		return err
	}
	return nil
--- end

# The filter dropped entirely, so an unverifiable digest is reported as a
# failure. This is the opposite error and costs a good extraction rather than
# hiding a bad one, so it needs its own case in the table.
[an unverifiable digest is treated as a failure]
file internal/unpack/go_unrar.go
--- anchor
	if err := entry.Close(); err != nil && !errors.Is(err, rarengine.ErrChecksumUnsupported) {
		return err
	}
	return nil
--- replace
	if err := entry.Close(); err != nil {
		return err
	}
	return nil
--- end

# Close is never called, so a member is left unfinished. errors.Is is kept in
# the replacement only to hold the import; dropping it would break the build,
# which says nothing about whether the test discriminates.
[the member is never closed]
file internal/unpack/go_unrar.go
--- anchor
	if err := entry.Close(); err != nil && !errors.Is(err, rarengine.ErrChecksumUnsupported) {
		return err
	}
	return nil
--- replace
	var err error
	if err != nil && !errors.Is(err, rarengine.ErrChecksumUnsupported) {
		return err
	}
	return nil
--- end
