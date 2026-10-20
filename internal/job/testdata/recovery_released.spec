pkg ./internal/job/
run ^TestRecoveryReleased$

# RecoveryReleased counts a job's recovery volumes by what was done to them.

[fetched is reported with no release reason]
file internal/job/progress.go
--- anchor
	if p.par2ReleaseReason != "" {
		fetched = total - held
--- replace
	if p.par2ReleaseReason != "" || true {
		fetched = total - held
--- end

[fetched is never reported]
file internal/job/progress.go
--- anchor
	if p.par2ReleaseReason != "" {
		fetched = total - held
--- replace
	if false {
		fetched = total - held
--- end

[a file that is not a recovery volume is counted]
file internal/job/progress.go
--- anchor
		if !m.IsPar2Recovery(fi) {
			continue
--- replace
		if false {
			continue
--- end

[only a held volume is counted as held, not a discarded one]
file internal/job/progress.go
--- anchor
		if p.files[fi].Fetch != FetchAlways {
			held++
--- replace
		if p.files[fi].Fetch == FetchIfNeeded {
			held++
--- end

[the total omits a volume]
file internal/job/progress.go
--- anchor
		total++
		if p.files[fi].Fetch != FetchAlways {
--- replace
		if p.files[fi].Fetch != FetchAlways {
--- end
