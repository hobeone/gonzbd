pkg ./scripts/gitscope/
run TestDiff_FailedUntrackedDiffIsAnError|TestDiff_UntrackedPathGitCannotAccessIsAnError|TestDiff_RendersUntrackedFileWithParseableHeader|TestUntrackedDiffErr

[Diff drops a failed untracked diff]
file scripts/gitscope/gitscope.go
--- anchor
		if err := untrackedDiffErr(out, err); err != nil {
--- replace
		if err := untrackedDiffErr(out, err); false && err != nil {
--- end

[untrackedDiffErr accepts any exit status]
file scripts/gitscope/gitscope.go
--- anchor
	if !ok || exitErr.ExitCode() != 1 || out == "" {
--- replace
	if !ok || exitErr.ExitCode() < 0 || out == "" {
--- end

[untrackedDiffErr rejects exit status 1, the expected "inputs differ" result]
file scripts/gitscope/gitscope.go
--- anchor
	if !ok || exitErr.ExitCode() != 1 || out == "" {
--- replace
	if !ok || exitErr.ExitCode() != 2 || out == "" {
--- end

[untrackedDiffErr accepts exit 1 with no output (git could not access the path)]
file scripts/gitscope/gitscope.go
--- anchor
	if !ok || exitErr.ExitCode() != 1 || out == "" {
--- replace
	if !ok || exitErr.ExitCode() != 1 {
--- end

[untrackedDiffErr accepts a failure that is not an ExitError]
file scripts/gitscope/gitscope.go
--- anchor
	if !ok || exitErr.ExitCode() != 1 || out == "" {
--- replace
	if ok && (exitErr.ExitCode() != 1 || out == "") {
--- end
