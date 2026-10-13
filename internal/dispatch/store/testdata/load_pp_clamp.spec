pkg ./internal/dispatch/store/
run TestStore_LoadSaturatesAnOutOfRangePP|TestStore_AllHeaderFieldsAndTimestampsSurviveRoundTrip

[Load drops the pp column]
file internal/dispatch/store/store.go
--- anchor
		p.Header.PP = job.ClampPP(pp)
--- replace
		p.Header.PP = job.ClampPP(0)
--- end

[Load skips a row whose pp is out of range]
file internal/dispatch/store/store.go
--- anchor
		p.Header.PP = job.ClampPP(pp)
--- replace
		if pp < 0 || pp > 3 {
			continue
		}
		p.Header.PP = job.ClampPP(pp)
--- end

[Save writes no level]
file internal/dispatch/store/store.go
--- anchor
		p.Header.PP.Int(), p.Header.NZBBackup, p.Header.URL, p.Header.MD5,
--- replace
		0, p.Header.NZBBackup, p.Header.URL, p.Header.MD5,
--- end
