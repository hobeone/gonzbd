pkg ./internal/app/
run TestRecordedFileName_SurvivesASanitizeChange|TestJobFileLocation_ResolvesTheRecordedNameVerbatim

[the resolver re-sanitizes the recorded name under the current options]
file internal/app/verify.go
--- anchor
	loc := jobFile{Dir: jobDir, Name: recorded}
--- replace
	loc := jobFile{Dir: jobDir, Name: fsutil.SanitizeFilename(recorded, p.sanitize)}
--- end

[a later registration names the file afresh instead of from its record]
file internal/app/pipeline.go
--- anchor
		if loc, err = p.jobFileLocation(name, filename); err != nil {
--- replace
		if loc = p.newJobFileLocation(name, filename); err != nil {
--- end

[the recorded name's one-component check dropped]
file internal/app/verify.go
--- anchor
	return loc, fsutil.CheckOneComponent(recorded)
--- replace
	return loc, nil
--- end

[the verifier ignores a name the resolver refuses]
file internal/app/verify.go
run ^TestVerifyJobFiles_ARefusedRecordedNameIsAFault$
--- anchor
		loc, err := locate(f.Filename)
		path := loc.Path()
		if err != nil {
--- replace
		loc, err := locate(f.Filename)
		path := loc.Path()
		if false && err != nil {
--- end

[registerFile ignores a recorded name the resolver refuses]
file internal/app/pipeline.go
run ^TestRegisterFile_ARefusedRecordedNameFailsTheRegistration$
--- anchor
		if loc, err = p.jobFileLocation(name, filename); err != nil {
--- replace
		if loc, err = p.jobFileLocation(name, filename); false && err != nil {
--- end

[the archive peek opens a recorded name the resolver refuses]
file internal/app/archive_peek.go
run ^TestPeek_ARefusedRecordedNameIsNotOpened$
--- anchor
		if loc, err := app.pipeline.jobFileLocation(j.Name(), name); err == nil {
--- replace
		if loc, err := app.pipeline.jobFileLocation(j.Name(), name); err == nil || true {
--- end
