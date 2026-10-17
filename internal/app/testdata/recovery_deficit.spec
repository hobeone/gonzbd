pkg ./internal/app/
run ^(TestRecoveryDeficit|TestMaybeReleaseRecoveryVolumes_SizedRelease)$

# recoveryDeficit sizes the release an outcomeRepair verdict makes. Each
# mutation below weakens one of its refusals or one term of its arithmetic; a
# refusal that stops firing sends a job whose damage cannot be sized down the
# sized path, which fetches too little.

[a CRC mismatch no longer makes the release unsized]
file internal/app/par2_release.go
--- anchor
	case a.CRC.Mismatched > 0:
--- replace
	case false:
--- end

[an unchecked par2 entry no longer makes the release unsized]
file internal/app/par2_release.go
--- anchor
	case a.CRC.Unverified > 0:
--- replace
	case false:
--- end

[two par2 sets are sized as if one slice size covered them]
file internal/app/par2_release.go
--- anchor
	sliceSize, ok := a.ID.SliceSize()
	if !ok {
--- replace
	sliceSize, ok := a.ID.SliceSize()
	if false && !ok {
--- end

[a file size beyond int64 is no longer refused]
file internal/app/par2_release.go
--- anchor
		if f.Desc.FileSize > math.MaxInt64 {
--- replace
		if false && f.Desc.FileSize > math.MaxInt64 {
--- end

[the slice cap is not enforced, so rows are read for an oversized set]
file internal/app/par2_release.go
--- anchor
		if total > maxPar2Slices {
--- replace
		if false {
--- end

[the slice cap excludes the limit itself]
file internal/app/par2_release.go
--- anchor
		if total > maxPar2Slices {
--- replace
		if total >= maxPar2Slices {
--- end

[an entry with no delivered file is no longer refused]
file internal/app/par2_release.go
--- anchor
	if slices.Contains(pos, -1) {
--- replace
	if false && slices.Contains(pos, -1) {
--- end

[a failure to read the written rows is ignored]
file internal/app/par2_release.go
--- anchor
	rows, err := loadRows()
	if err != nil {
--- replace
	rows, err := loadRows()
	if false && err != nil {
--- end

[a held volume counts toward the blocks already fetched]
file internal/app/par2_release.go
--- anchor
		if !m.FileIsPar2Recovery(fi) || p.FileFetchPolicy(fi) != job.FetchAlways || p.FileFailedBytes(fi) > 0 {
--- replace
		if !m.FileIsPar2Recovery(fi) || p.FileFailedBytes(fi) > 0 {
--- end

[a fetched volume with a failed article counts toward the blocks already fetched]
file internal/app/par2_release.go
--- anchor
		if !m.FileIsPar2Recovery(fi) || p.FileFetchPolicy(fi) != job.FetchAlways || p.FileFailedBytes(fi) > 0 {
--- replace
		if !m.FileIsPar2Recovery(fi) || p.FileFetchPolicy(fi) != job.FetchAlways || false {
--- end

[the blocks already fetched are not subtracted]
file internal/app/par2_release.go
--- anchor
	return job.NeedBlocks(damaged - available), ""
--- replace
	return job.NeedBlocks(damaged), ""
--- end

# maybeReleaseRecoveryVolumes acts on the sizing.

[an outcomeRepair verdict never sizes the release]
file internal/app/app.go
--- anchor
			if outcome == outcomeRepair {
--- replace
			if false {
--- end

[the release ignores the sizing]
file internal/app/app.go
--- anchor
		n, err := app.releaseRecoveryVolumes(j, reason, sizing)
--- replace
		n, err := app.releaseRecoveryVolumes(j, reason, job.Unsized())
		_ = sizing
--- end

[a release that frees nothing still reports a fetch]
file internal/app/app.go
--- anchor
		if n == 0 {
--- replace
		if false {
--- end

[an unsizable release does not say why it freed everything]
file internal/app/app.go
--- anchor
				if why != "" {
--- replace
				if false {
--- end

[a missing durability store is not refused]
file internal/app/app.go
--- anchor
					if app.durable == nil {
--- replace
					if false {
--- end
