pkg ./internal/job/
run TestAckDurable|TestAckPermanentFailure|TestUndeferRecovery|TestSeedFromRuns|TestReplaceFromRuns

[the mis-routed-proof check neutered]
file internal/job/workset.go
--- anchor
	if proofJobID != j.id {
--- replace
	if false {
--- end

[the out-of-range report dropped]
file internal/job/workset.go
--- anchor
	if invalid > 0 {
--- replace
	if false {
--- end

[the on-demand par2 release skipped]
file internal/job/workset.go
--- anchor
			if !p.par2Recovered && m.RecoveryFiles() > 0 {
--- replace
			if false {
--- end

[ReplaceFromRuns' file-index validation neutered]
file internal/job/workset.go
--- anchor
			if f < 0 || f >= m.NumFiles() {
--- replace
			if false {
--- end

[ReplaceFromRuns stops clearing Complete and the CRC]
file internal/job/workset.go
--- anchor
			if fileCleared > 0 {
--- replace
			if false {
--- end

[undeferRecovery stops skipping a volume that is not held]
file internal/job/workset.go
--- anchor
		if fi < 0 || fi >= m.NumFiles() || p.files[fi].Fetch != FetchIfNeeded {
--- replace
		if false {
--- end
