pkg ./internal/dispatch/
run TestSetName_RefusesUnsafeOrTakenNames

[a name that is not one path component is stored]
file internal/dispatch/registry.go
--- anchor
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
--- replace
	if false {
--- end

[a separator is allowed]
file internal/dispatch/registry.go
--- anchor
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
--- replace
	if name == "" || name == "." || name == ".." {
--- end

[dot-dot is allowed]
file internal/dispatch/registry.go
--- anchor
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
--- replace
	if name == "" || name == "." || strings.ContainsAny(name, "/\\\x00") {
--- end

[another job's name is allowed]
file internal/dispatch/registry.go
--- anchor
		if otherID != id && other.h.Name == name {
--- replace
		if false && otherID != id && other.h.Name == name {
--- end

[the job's own name is refused]
file internal/dispatch/registry.go
--- anchor
		if otherID != id && other.h.Name == name {
--- replace
		if other.h.Name == name {
--- end
