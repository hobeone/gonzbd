pkg ./internal/assembler/
run TestOpenTargetFile_ClosedJobDropsTheArticle$

# An article whose resolver answers ErrJobClosed is dropped silently; it is
# not a storage fault and no file is opened.

[the closed-job branch in processRequest is unreachable]
file internal/assembler/assembler.go
--- anchor
		if errors.Is(err, ErrJobClosed) {
			// The resolver says the job no longer accepts writes. Dropped as
--- replace
		if false {
			// The resolver says the job no longer accepts writes. Dropped as
--- end
