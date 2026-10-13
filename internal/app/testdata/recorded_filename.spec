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
