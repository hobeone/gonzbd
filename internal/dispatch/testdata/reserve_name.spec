pkg ./internal/dispatch/
run TestReserveName_

# A reservation refuses every job but its holder.
[the registry ignores reservations when testing a name]
file internal/dispatch/registry.go
--- anchor
	if holder, ok := d.reservedNames[name]; ok && holder != id {
--- replace
	if holder, ok := d.reservedNames[name]; ok && holder != id && false {
--- end

# A release frees only its own reservation.
[a release frees whatever holds the name]
file internal/dispatch/registry.go
--- anchor
		if d.reservedNames[name] == id {
--- replace
		if true {
--- end

# The holder's own registration is not refused by its reservation.
[the holder is refused its own name]
file internal/dispatch/registry.go
--- anchor
	if holder, ok := d.reservedNames[name]; ok && holder != id {
--- replace
	if holder, ok := d.reservedNames[name]; ok {
--- end
