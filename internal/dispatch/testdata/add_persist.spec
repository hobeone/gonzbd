pkg ./internal/dispatch/
run TestAdd_WritesTheQueueRowBeforeReturning|TestAdd_WritesUnderTheCallersContext|TestAdd_TickNeverSeesAJobWhoseRowIsUnwritten|TestAdd_AbortedRemovalDuringTheWriteLeavesTheJobVisible|TestAdd_KicksTheTickAfterThePersist|TestStart_AfterAddKeepsTheJobRegisteredOnce|TestStart_RefusesARowThatDiffersFromTheOneAddWrote

[snapshotOrder no longer skips jobs being added]
file internal/dispatch/registry.go
--- anchor
		if _, ok := d.adding[id]; ok {
--- replace
		if false {
--- end

[snapshotOrder gates on d.written instead of d.adding, as design section 3.1 has it]
file internal/dispatch/registry.go
--- anchor
		if _, ok := d.adding[id]; ok {
--- replace
		if _, ok := d.written[id]; !ok {
--- end

[register no longer marks an Add's job as being added]
file internal/dispatch/registry.go
--- anchor
		d.adding[j.ID()] = struct{}{}
--- replace
--- end

[Add never clears its job from d.adding]
file internal/dispatch/registry.go
--- anchor
	delete(d.adding, j.ID())
	d.mu.Unlock()
--- replace
	d.mu.Unlock()
--- end

[Add no longer writes the row]
file internal/dispatch/registry.go
--- anchor
	if err := d.persistIfChanged(ctx, j); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end

[Add writes under a context other than its caller's]
file internal/dispatch/registry.go
--- anchor
	if err := d.persistIfChanged(ctx, j); err != nil {
--- replace
	if err := d.persistIfChanged(context.Background(), j); err != nil {
--- end

[a failed write no longer unwinds the registration]
file internal/dispatch/registry.go
--- anchor
		if rm, ok := d.beginRemoval(j.ID()); ok {
--- replace
		if rm, ok := (*removal)(nil), false; ok {
--- end

[no kick after the write]
file internal/dispatch/registry.go
--- anchor
	// skipped the job as still being added.
	d.kick()
--- replace
	// skipped the job as still being added.
--- end

[restore re-registers a row this process already wrote]
file internal/dispatch/dispatch.go
--- anchor
		if w, ok := d.lastWritten(p.ID); ok && w == p {
--- replace
		if false {
--- end

[restore skips any registered job's row, not only the one this process wrote]
file internal/dispatch/dispatch.go
--- anchor
		if w, ok := d.lastWritten(p.ID); ok && w == p {
--- replace
		if _, ok := d.lastWritten(p.ID); ok {
--- end
