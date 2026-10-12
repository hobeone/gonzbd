pkg ./internal/config/
run TestCategoryConfig_validate_RejectsFlatDir|TestConfigValidate_NamesFlatCategory

[the trailing-star refusal is removed]
file internal/config/validate.go
--- anchor
	if before, ok := strings.CutSuffix(c.Dir, "*"); ok {
--- replace
	if before, ok := "", false; ok {
--- end
