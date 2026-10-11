pkg ./internal/config/
run TestResolvePriority

[the category's own priority is not consulted]
file internal/config/categories.go
--- anchor
	for _, name := range []string{category, ""} {
--- replace
	for _, name := range []string{""} {
--- end

[the fallback category is not consulted]
file internal/config/categories.go
--- anchor
	for _, name := range []string{category, ""} {
--- replace
	for _, name := range []string{category} {
--- end

[a sentinel survives the chain]
file internal/config/categories.go
--- anchor
	return constants.NormalPriority, pause
}
--- replace
	return p, pause
}
--- end

[a Paused category priority does not pause]
file internal/config/categories.go
--- anchor
		if c == constants.PausedPriority {
			pause = true
--- replace
		if c == constants.PausedPriority {
			pause = false
--- end

[a Paused category priority is held as the job's priority]
file internal/config/categories.go
--- anchor
		if c == constants.PausedPriority {
--- replace
		if false {
--- end

[a Paused request does not pause]
file internal/config/categories.go
--- anchor
	pause := p == constants.PausedPriority
--- replace
	pause := false
--- end

[a Paused request is held unresolved]
file internal/config/categories.go
--- anchor
	if p != constants.DefaultPriority && !pause {
--- replace
	if p != constants.DefaultPriority {
--- end

[DefaultPriority counts as a category priority]
file internal/config/categories.go
--- anchor
	if !p.IsConcrete() && p != constants.PausedPriority {
--- replace
	if !p.IsConcrete() && p != constants.PausedPriority && p != constants.DefaultPriority {
--- end

[StopPriority counts as a category priority]
file internal/config/categories.go
--- anchor
	if !p.IsConcrete() && p != constants.PausedPriority {
--- replace
	if !p.IsConcrete() && p != constants.PausedPriority && p != constants.StopPriority {
--- end

[an unknown value counts as a category priority]
file internal/config/categories.go
--- anchor
	if !p.IsConcrete() && p != constants.PausedPriority {
--- replace
	if !p.IsConcrete() && p != constants.PausedPriority && p != 7 {
--- end

[an out-of-range value counts as a category priority]
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
	if p != constants.DefaultPriority && !pause {
		return p, false
	}
--- replace
	if false {
		return p, false
	}
--- end
