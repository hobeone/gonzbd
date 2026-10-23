pkg ./internal/app/
run Test(ResolveFileInfo_RefusesAJobThatLeftTheDispatcher|ResolveFileInfo_RefusesAHandedOffJob|ResolveFileInfo_RefusesAnEarlierInstance|RegisterFile_RefusesOnceTheJobLeftTheDispatcher|HandleSuccessResult_DropsAClosedJobsArticleWithoutReturningIt)$

# resolveForWrite is the gate that replaced the assembler's job-level
# tombstone (#1053); the register guard keeps a late article from re-growing
# fileInfo; the handler treats a closed-job refusal as the instance-check drop.

[the resolver ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	if cur, live := p.dispatcher.Job(jobID); !live || cur != entry.j {
--- replace
	if false {
--- end

[the resolver ignores the hand-over]
file internal/app/pipeline.go
--- anchor
	if p.handedOff != nil && p.handedOff(entry.j) {
--- replace
	if false {
--- end

[registerFile ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	if cur, live := p.dispatcher.Job(jobID); !live || cur != j {
--- replace
	if false {
--- end

[the success handler returns a closed job's article to the pool]
file internal/app/pipeline.go
--- anchor
		if errors.Is(err, assembler.ErrJobClosed) {
			// The instance check at the top of handleResult, re-run under
--- replace
		if false {
			// The instance check at the top of handleResult, re-run under
--- end
