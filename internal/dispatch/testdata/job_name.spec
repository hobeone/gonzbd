pkg ./internal/dispatch/
run TestSetName_RefusesUnsafeOrTakenNames|TestAdd_RefusesANameAnotherJobHas

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

[registration does not check the name]
file internal/dispatch/registry.go
--- anchor
	if otherID, taken := d.nameHolderLocked(j.ID(), h.Name); taken {
--- replace
	if otherID, taken := d.nameHolderLocked(j.ID(), h.Name); false && taken {
--- end

[a rename does not check the name]
file internal/dispatch/registry.go
--- anchor
	if otherID, taken := d.nameHolderLocked(id, name); taken {
--- replace
	if otherID, taken := d.nameHolderLocked(id, name); false && taken {
--- end

[a taken name is refused as merely invalid]
file internal/dispatch/registry.go
--- anchor
		return fmt.Errorf("dispatch: register %s as %q: job %s has that name: %w", j.ID(), h.Name, otherID, ErrJobNameTaken)
--- replace
		return fmt.Errorf("dispatch: register %s as %q: job %s has that name: %w", j.ID(), h.Name, otherID, ErrInvalidJobName)
--- end
