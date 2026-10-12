pkg ./internal/dispatch/
run ^(TestAdd_RefusesANameThatIsNotOnePathComponent|TestReserveName_RefusesANameThatIsNotOnePathComponent|TestRestore_SkipsARowWhoseNameIsNotOnePathComponent|TestSetName_RefusesUnsafeOrTakenNames)$

# Every door a name enters the registry through refuses one that is not a
# single safe path component: admitNameLocked owns the test, and register,
# ReserveName and SetName each call it.

[the registry admits a separator]
file internal/dispatch/registry.go
--- anchor
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
--- replace
	if name == "" || name == "." || name == ".." || (false && strings.ContainsAny(name, "/\\\x00")) {
--- end

[registration does not apply the name check]
file internal/dispatch/registry.go
--- anchor
	if err := d.admitNameLocked(j.ID(), h.Name); err != nil {
--- replace
	if err := d.admitNameLocked(j.ID(), h.Name); false && err != nil {
--- end

[a reservation does not apply the name check]
file internal/dispatch/registry.go
--- anchor
	if err := d.admitNameLocked(id, name); err != nil {
		return nil, fmt.Errorf("dispatch: reserve for %s: %w", id, err)
--- replace
	if err := d.admitNameLocked(id, name); false && err != nil {
		return nil, fmt.Errorf("dispatch: reserve for %s: %w", id, err)
--- end

[a rename does not apply the name check]
file internal/dispatch/registry.go
--- anchor
	if err := d.admitNameLocked(id, name); err != nil {
		return fmt.Errorf("dispatch: set name %s: %w", id, err)
--- replace
	if err := d.admitNameLocked(id, name); false && err != nil {
		return fmt.Errorf("dispatch: set name %s: %w", id, err)
--- end

# The job's name is what its directory is joined from, so it is the one
# checked: register takes the header's name from it.
[a job named unsafely registers under a safe header name]
file internal/dispatch/registry.go
--- anchor
	h.Name = j.Name()
--- replace
	_ = j.Name()
--- end
