pkg ./internal/app/
run ^(TestRecoveryDeficit|TestMaybeReleaseRecoveryVolumes_SizedRelease)$

# recoveryDeficit sizes the release an outcomeRepair verdict makes. Each
# mutation below weakens one of its refusals or one term of its arithmetic; a
# refusal that stops firing sends a job whose damage cannot be sized down the
# sized path, which fetches too little.

[a CRC mismatch is sized from the rows alone, with no block-level verify]
file internal/app/par2_release.go
--- anchor
	verified := a.CRC.Mismatched > 0 || a.CRC.Unverified > 0
--- replace
	verified := a.CRC.Unverified > 0
--- end

[an unchecked par2 entry is sized from the rows alone, with no block-level verify]
file internal/app/par2_release.go
--- anchor
	verified := a.CRC.Mismatched > 0 || a.CRC.Unverified > 0
--- replace
	verified := a.CRC.Mismatched > 0
--- end

[no damage is sized by a block-level verify]
file internal/app/par2_release.go
--- anchor
	verified := a.CRC.Mismatched > 0 || a.CRC.Unverified > 0
--- replace
	verified := false
--- end

[a block-level verify runs for every release]
file internal/app/par2_release.go
--- anchor
	verified := a.CRC.Mismatched > 0 || a.CRC.Unverified > 0
--- replace
	verified := true
--- end

[an entry no File Description describes no longer makes the release unsized]
file internal/app/par2_release.go
--- anchor
		if fd.Undescribed {
--- replace
		if false {
--- end

[the entries nothing delivered matched are left out of the expected data blocks]
file internal/app/par2_release.go
--- anchor
		sizes = append(sizes, fd.FileSize)
	}
	var total
--- replace
		_ = fd
	}
	var total
--- end

[a verify that counts a different number of data blocks is trusted]
file internal/app/par2_release.go
--- anchor
		if uint64(res.DataBlocks) != total {
--- replace
		if false {
--- end

[a verify that counts fewer data blocks than the index describes is trusted]
file internal/app/par2_release.go
--- anchor
		if uint64(res.DataBlocks) != total {
--- replace
		if uint64(res.DataBlocks) > total {
--- end

[a verify that counts more data blocks than the index describes is trusted]
file internal/app/par2_release.go
--- anchor
		if uint64(res.DataBlocks) != total {
--- replace
		if uint64(res.DataBlocks) < total {
--- end

[a failed block-level verify is ignored]
file internal/app/par2_release.go
--- anchor
		res, err := verify()
		if err != nil {
--- replace
		res, err := verify()
		if false && err != nil {
--- end

[unreadable rows under a block-level verify make the release unsized]
file internal/app/par2_release.go
--- anchor
		if verified {
			return job.NeedBlocks(verifiedNeed), ""
--- replace
		if false {
			return job.NeedBlocks(verifiedNeed), ""
--- end

[a block-level verify is sized by the rows alone]
file internal/app/par2_release.go
--- anchor
	return job.NeedBlocks(max(verifiedNeed, damaged-p.FetchedRecoveryBlocks(m))), ""
--- replace
	return job.NeedBlocks(damaged - p.FetchedRecoveryBlocks(m)), ""
--- end

[the rows are sized by the verify alone]
file internal/app/par2_release.go
--- anchor
	return job.NeedBlocks(max(verifiedNeed, damaged-p.FetchedRecoveryBlocks(m))), ""
--- replace
	return job.NeedBlocks(verifiedNeed), ""
--- end

[the blocks already fetched are subtracted from the verify]
file internal/app/par2_release.go
--- anchor
	return job.NeedBlocks(max(verifiedNeed, damaged-p.FetchedRecoveryBlocks(m))), ""
--- replace
	return job.NeedBlocks(max(verifiedNeed-p.FetchedRecoveryBlocks(m), damaged-p.FetchedRecoveryBlocks(m))), ""
--- end

[a verified file with no delivered copy is read from the rows]
file internal/app/par2_release.go
--- anchor
		if pos[i] >= 0 {
--- replace
		if true {
--- end

[a verified release refuses an entry with no delivered file]
file internal/app/par2_release.go
--- anchor
	if !verified && slices.Contains(pos, -1) {
--- replace
	if slices.Contains(pos, -1) {
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
		if size > math.MaxInt64 {
--- replace
		if false && size > math.MaxInt64 {
--- end

[the slice cap is not enforced, so rows are read for an oversized set]
file internal/app/par2_release.go
--- anchor
		if total > job.MaxPar2Slices {
--- replace
		if false {
--- end

[the slice cap excludes the limit itself]
file internal/app/par2_release.go
--- anchor
		if total > job.MaxPar2Slices {
--- replace
		if total >= job.MaxPar2Slices {
--- end

[an entry with no delivered file is no longer refused]
file internal/app/par2_release.go
--- anchor
	if !verified && slices.Contains(pos, -1) {
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

[a file's rows are read by its identification index, not its manifest index]
file internal/app/par2_release.go
--- anchor
			damaged += job.DamagedSlices(byFile[pos[i]], f.Desc.FileSize, sliceSize)
--- replace
			damaged += job.DamagedSlices(byFile[i], f.Desc.FileSize, sliceSize)
--- end

[the blocks already fetched are not subtracted]
file internal/app/par2_release.go
--- anchor
	return job.NeedBlocks(max(verifiedNeed, damaged-p.FetchedRecoveryBlocks(m))), ""
--- replace
	return job.NeedBlocks(max(verifiedNeed, damaged)), ""
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

[a block-level verify of several par2 sets is run on the first]
file internal/app/app.go
--- anchor
					if len(sets) != 1 {
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
