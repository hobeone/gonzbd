pkg ./internal/app/
run Test(ResolveFileInfo_RefusesAJobThatLeftTheDispatcher|ResolveFileInfo_RefusesAHandedOffJob|ResolveFileInfo_RefusesAnEarlierInstance|RegisterFile_RefusesOnceTheJobLeftTheDispatcher|HandleSuccessResult_DropsAClosedJobsArticleWithoutReturningIt|New_WiresTheHandOverIntoTheResolver)$

# resolveForWrite is the assembler's gate on a closed job (#1053); the
# register guard keeps a late article from re-growing fileInfo; the handler
# treats a closed-job refusal as the instance-check drop; and New wires the
# hand-over half of the gate, which a literal can silently omit.

[the resolver ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	} else if !p.holds(j) {
--- replace
	} else if false {
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
--- replace
	if false {
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
