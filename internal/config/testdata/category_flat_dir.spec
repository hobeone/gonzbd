pkg ./internal/config/
run TestCategoryConfig_validate_RejectsFlatDir|TestConfigValidate_NamesFlatCategory

[the trailing-star refusal is removed]
file internal/config/validate.go
--- anchor
	if strings.HasSuffix(c.Dir, "*") {
--- replace
	if false {
--- end
