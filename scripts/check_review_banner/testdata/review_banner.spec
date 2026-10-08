pkg ./scripts/check_review_banner/
run TestCheckDir_SkipsOnlyWhenAllowMissingIsTrue

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
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
--- replace
		if _, err := os.Stat(dir); true || errors.Is(err, os.ErrNotExist) {
--- end
