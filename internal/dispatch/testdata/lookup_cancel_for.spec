pkg ./internal/dispatch/
run ^(TestLookupFor_MatchesByInstanceOnlyWhenAsked|TestCancelFor_LatchesOnlyTheJobItFinds)$

[lookupFor ignores the expected instance]
file internal/dispatch/dispatch.go
--- anchor
	if !ok || (expected != nil && e.j != expected) {
--- replace
	if !ok {
--- end

[cancelFor drops the expected instance on its lookup]
file internal/dispatch/dispatch.go
--- anchor
	j, ok := d.lookupFor(id, expected)
	if !ok {
		return fmt.Errorf("dispatch: Cancel: no job %q: %w", id, ErrNotFound)
--- replace
	j, ok := d.lookupFor(id, nil)
	if !ok {
		return fmt.Errorf("dispatch: Cancel: no job %q: %w", id, ErrNotFound)
--- end

[cancelFor never latches the intent]
file internal/dispatch/dispatch.go
--- anchor
	if err := d.q.Cancel(j); err != nil {
--- replace
	if err := error(nil); j == nil && err != nil {
--- end
