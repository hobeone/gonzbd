pkg ./internal/dispatch/
run TestSetName_RefusesUnsafeOrTakenNames|TestAdd_RefusesANameAnotherJobHas

[a name that is not one path component is stored]
file internal/dispatch/registry.go
--- anchor
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
--- replace
	if false && strings.ContainsAny(name, "/\\\x00") {
--- end

[a separator is allowed]
file internal/dispatch/registry.go
--- anchor
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
--- replace
	if name == "" || name == "." || name == ".." || (false && strings.ContainsAny(name, "/\\\x00")) {
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
	if err := d.admitNameLocked(j.ID(), h.Name); err != nil {
--- replace
	if err := d.admitNameLocked(j.ID(), h.Name); false && err != nil {
--- end

[a rename does not check the name]
file internal/dispatch/registry.go
--- anchor
	if err := d.admitNameLocked(id, name); err != nil {
		return fmt.Errorf("dispatch: set name %s: %w", id, err)
--- replace
	if err := d.admitNameLocked(id, name); false && err != nil {
		return fmt.Errorf("dispatch: set name %s: %w", id, err)
--- end

[a taken name is refused as merely invalid]
file internal/dispatch/registry.go
--- anchor
		return fmt.Errorf("name %q: job %s has it: %w", name, otherID, ErrJobNameTaken)
--- replace
		return fmt.Errorf("name %q: job %s has it: %w", name, otherID, ErrInvalidJobName)
--- end
