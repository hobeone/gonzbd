pkg ./internal/dispatch/
run TestOccupy_RemoveBypassesSelfDeadlock

[self-deadlock bypass in Remove neutered]
file internal/dispatch/registry.go
--- anchor
	if tok, ok := ctx.Value(occupyContextKey{}).(occupyToken); ok && tok.id == id {
--- replace
	if tok, _ := ctx.Value(occupyContextKey{}).(occupyToken); false {
--- end
