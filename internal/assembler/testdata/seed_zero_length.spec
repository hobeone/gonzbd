pkg ./internal/assembler/
run ^(TestOpenTargetFile_ZeroLengthOwnedRowIsSilent|TestOpenTargetFile_SeedsFromOwnedAndDropsInvalid)$

# seedOwned skips a zero-length row silently and still warns for a malformed
# range.

[a zero-length row is treated as malformed again]
file internal/assembler/assembler.go
--- anchor
		if r.Off < 0 || r.Len < 0 ||
--- replace
		if r.Off < 0 || r.Len <= 0 ||
--- end

[a malformed range is dropped without a warning]
file internal/assembler/assembler.go
--- anchor
			a.log.Warn("dropping invalid owned range",
--- replace
			a.log.Debug("dropping invalid owned range",
--- end
