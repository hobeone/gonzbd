pkg ./scripts/gitscope/
run TestDiff_FailedUntrackedDiffIsAnError|TestDiff_RendersUntrackedFileWithParseableHeader

[Diff drops a failed untracked diff]
file scripts/gitscope/gitscope.go
--- anchor
		if exitErr, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || exitErr.ExitCode() != 1) {
--- replace
		if exitErr, ok := errors.AsType[*exec.ExitError](err); false && err != nil && (!ok || exitErr.ExitCode() != 1) {
--- end

[Diff accepts any exit status from the untracked diff]
file scripts/gitscope/gitscope.go
--- anchor
		if exitErr, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || exitErr.ExitCode() != 1) {
--- replace
		if exitErr, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || exitErr.ExitCode() < 0) {
--- end

[Diff rejects exit status 1, the expected "inputs differ" result]
file scripts/gitscope/gitscope.go
--- anchor
		if exitErr, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || exitErr.ExitCode() != 1) {
--- replace
		if exitErr, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || exitErr.ExitCode() != 2) {
--- end
