pkg ./scripts/check_review_banner/
run TestCheckDir_SkipsOnlyWhenAllowMissingIsTrue|TestResolveArgs_DistinguishesDefaultFromExplicitDir

[checkDir never skips a missing default directory]
file scripts/check_review_banner/main.go
--- anchor
	if allowMissing {
--- replace
	if false && allowMissing {
--- end

[checkDir skips a missing explicit directory even when allowMissing is false]
file scripts/check_review_banner/main.go
--- anchor
	if allowMissing {
--- replace
	if true || allowMissing {
--- end

[checkDir reports skipped even when the directory exists]
file scripts/check_review_banner/main.go
--- anchor
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) { //nolint:gosec // G703: dir is the operator's -dir flag or repoRoot/docs/reviews
--- replace
		if _, err := os.Stat(dir); true || errors.Is(err, os.ErrNotExist) { //nolint:gosec // G703: dir is the operator's -dir flag or repoRoot/docs/reviews
--- end

[resolveArgs ignores an explicit -dir flag]
file scripts/check_review_banner/main.go
--- anchor
		if f.Name == "dir" {
--- replace
		if false && f.Name == "dir" {
--- end

[resolveArgs treats the default directory as explicit]
file scripts/check_review_banner/main.go
--- anchor
	return *dirFlag, !explicitDir, nil
--- replace
	return *dirFlag, false && !explicitDir, nil
--- end

