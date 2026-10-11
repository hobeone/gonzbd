pkg ./internal/config/
run TestResolvePriority

[the category's own priority is not consulted]
file internal/config/categories.go
--- anchor
	if c, ok := concretePriority(FindCategory(cats, category).Priority); ok {
--- replace
	if c, ok := concretePriority(FindCategory(cats, category).Priority); ok && false {
--- end

[the fallback category is not consulted]
file internal/config/categories.go
--- anchor
	if c, ok := concretePriority(FindCategory(cats, "").Priority); ok {
--- replace
	if c, ok := concretePriority(FindCategory(cats, "").Priority); ok && false {
--- end

[a sentinel survives the chain]
file internal/config/categories.go
--- anchor
	return constants.NormalPriority
}
--- replace
	return p
}
--- end

[DefaultPriority counts as concrete]
file internal/config/categories.go
--- anchor
	if !p.IsValid() || p == constants.DefaultPriority || p == constants.StopPriority {
--- replace
	if !p.IsValid() || p == constants.StopPriority {
--- end

[StopPriority counts as concrete]
file internal/config/categories.go
--- anchor
	if !p.IsValid() || p == constants.DefaultPriority || p == constants.StopPriority {
--- replace
	if !p.IsValid() || p == constants.DefaultPriority {
--- end

[an unknown value counts as concrete]
file internal/config/categories.go
--- anchor
	if !p.IsValid() || p == constants.DefaultPriority || p == constants.StopPriority {
--- replace
	if p == constants.DefaultPriority || p == constants.StopPriority {
--- end

[an out-of-range value counts as concrete]
file internal/config/categories.go
--- anchor
	if v < -128 || v > 127 {
		return 0, false
	}
--- replace
	if false {
		return 0, false
	}
--- end

[an explicit request is resolved anyway]
file internal/config/categories.go
--- anchor
	if p != constants.DefaultPriority {
		return p
	}
--- replace
	if false {
		return p
	}
--- end
