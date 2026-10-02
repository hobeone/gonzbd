pkg ./internal/history/
run TestAddGetRoundTrip_EveryFieldDistinct

[AddTx drops the unwanted state]
file internal/history/repository.go
--- anchor
		e.Archive, toUnix(e.TimeAdded), e.NZBBackup, e.Unwanted,
--- replace
		e.Archive, toUnix(e.TimeAdded), e.NZBBackup, 0,
--- end
