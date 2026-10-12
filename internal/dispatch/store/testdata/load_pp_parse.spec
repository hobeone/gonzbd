pkg ./internal/dispatch/store/
run TestStore_LoadSkipsAnOutOfRangePP|TestStore_AllHeaderFieldsAndTimestampsSurviveRoundTrip

[Load saturates the pp column instead of refusing it]
file internal/dispatch/store/store.go
--- anchor
		if p.Header.PP, err = job.ParsePP(pp); err != nil {
--- replace
		if p.Header.PP, err = job.ClampPP(pp), error(nil); err != nil {
--- end

[Load ignores ParsePP's refusal]
file internal/dispatch/store/store.go
--- anchor
		if p.Header.PP, err = job.ParsePP(pp); err != nil {
--- replace
		if p.Header.PP, err = job.ParsePP(pp); err != nil && false {
--- end

[Save writes no level]
file internal/dispatch/store/store.go
--- anchor
		p.Header.PP.Int(), p.Header.NZBBackup, p.Header.URL, p.Header.MD5,
--- replace
		0, p.Header.NZBBackup, p.Header.URL, p.Header.MD5,
--- end
