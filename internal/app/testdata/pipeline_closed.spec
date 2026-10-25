pkg ./internal/app/
run Test(ResolveFileInfo_RefusesAJobThatLeftTheDispatcher|RegisterFile_ReplacesAnEarlierInstancesEntry|ResolveFileInfo_RefusesAHandedOffJob|ResolveFileInfo_RefusesAnEarlierInstance|RegisterFile_RefusesOnceTheJobLeftTheDispatcher|HandleSuccessResult_DropsAClosedJobsArticleWithoutReturningIt|New_WiresTheHandOverIntoTheResolver)$

# resolveForWrite is the assembler's gate on a closed job (#1053); the
# register guard keeps a late article from re-growing fileInfo; the handler
# treats a closed-job refusal as the instance-check drop; and New wires the
# hand-over half of the gate, which a literal can silently omit.

[the resolver ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	if !p.holds(j) {
		return assembler.FileInfo{}, fmt.Errorf("resolve %s[%d]: instance left the dispatcher: %w", j.ID(), fileIdx, assembler.ErrJobClosed)
--- replace
	if false {
		return assembler.FileInfo{}, fmt.Errorf("resolve %s[%d]: instance left the dispatcher: %w", j.ID(), fileIdx, assembler.ErrJobClosed)
--- end

[the resolver ignores the hand-over]
file internal/app/pipeline.go
--- anchor
	if p.handedOff != nil && p.handedOff(j) {
--- replace
	if false {
--- end

[registerFile ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	if !p.holds(j) {
		p.mu.Unlock()
		return fmt.Errorf("register %s[%d]: instance left the dispatcher: %w", jobID, fileIdx, assembler.ErrJobClosed)
--- replace
	if false {
		p.mu.Unlock()
		return fmt.Errorf("register %s[%d]: instance left the dispatcher: %w", jobID, fileIdx, assembler.ErrJobClosed)
--- end

[the handler returns a closed job's article to the pool]
file internal/app/pipeline.go
--- anchor
	if errors.Is(err, assembler.ErrJobClosed) {
		p.log.Debug("dropping "+what+" for a job instance that left the dispatcher while its file was being registered",
--- replace
	if false {
		p.log.Debug("dropping "+what+" for a job instance that left the dispatcher while its file was being registered",
--- end

[New leaves the hand-over out of the resolver]
file internal/app/app.go
--- anchor
		handedOff: app.postProcAdmissions.has,
--- replace
		// mutated: hand-over not wired
--- end

[registerFile's fast path defers to any instance's entry]
file internal/app/pipeline.go
--- anchor
	entry, exists := p.fileInfo[key]
	p.mu.RUnlock()
	if exists && entry.j == j {
--- replace
	_, exists := p.fileInfo[key]
	p.mu.RUnlock()
	if exists {
--- end

[registerFileAttempt's double-check defers to any instance's entry]
file internal/app/pipeline.go
--- anchor
	// the race between RUnlock and Lock.
	if entry, exists := p.fileInfo[key]; exists && entry.j == j {
--- replace
	// the race between RUnlock and Lock.
	if _, exists := p.fileInfo[key]; exists {
--- end

[registerFileAttempt's third check defers to any instance's entry]
file internal/app/pipeline.go
--- anchor
	// nothing forgot; this instance's registration replaces it.
	if entry, exists := p.fileInfo[key]; exists && entry.j == j {
--- replace
	// nothing forgot; this instance's registration replaces it.
	if _, exists := p.fileInfo[key]; exists {
--- end
