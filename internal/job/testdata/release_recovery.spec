pkg ./internal/job/
run TestCheapestCover|TestDamagedSlices|TestReleaseRecovery

[insufficient total no longer refused up front]
file internal/job/cover.go
--- anchor
	if total < need {
--- replace
	if false {
--- end

[the capped total is not capped]
file internal/job/cover.go
--- anchor
			nc := min(need, c+b)
--- replace
			nc := min(need+b, c+b)
--- end

[volumes may be reused: upward iteration]
file internal/job/cover.go
--- anchor
		for c := need; c >= 0; c-- {
--- replace
		for c := 0; c < need; c++ {
--- end

[a non-positive need is no longer the empty cover]
file internal/job/cover.go
--- anchor
	if need <= 0 {
--- replace
	if false {
--- end

[a hopeless job still releases]
file internal/job/release.go
--- anchor
HasPar2Files()).Hopeless() {
--- replace
HasPar2Files()).Hopeless() && false {
--- end

[the zero value is treated as sized]
file internal/job/release.go
--- anchor
	if s.sized && j.manifest != nil {
--- replace
	if j.manifest != nil {
--- end

[an evicted manifest is dereferenced]
file internal/job/release.go
--- anchor
	if s.sized && j.manifest != nil {
--- replace
	if s.sized {
--- end

[every candidate counts as one block]
file internal/job/release.go
--- anchor
				blocks = append(blocks, n)
--- replace
				blocks = append(blocks, min(n, 1))
--- end

[candidate bytes ignored]
file internal/job/release.go
--- anchor
				bytes = append(bytes, j.manifest.FileBytes(i))
--- replace
				bytes = append(bytes, 1)
--- end

[an insufficient cover releases only the cover]
file internal/job/release.go
--- anchor
		if cover, ok := cheapestCover(blocks, bytes, s.need); ok {
--- replace
		if cover, ok := cheapestCover(blocks, bytes, s.need); ok || true {
--- end

[a slice shared by two runs is counted twice]
file internal/job/verified.go
--- anchor
		if lo <= lastCounted {
--- replace
		if false {
--- end

[the tail run is dropped]
file internal/job/verified.go
--- anchor
	if frontier < f {
--- replace
	if false {
--- end

[a gap before a row past the end is not clamped]
file internal/job/verified.go
--- anchor
			run(frontier, min(r.Offset, f))
--- replace
			run(frontier, r.Offset)
--- end

[rows after the file is covered are still walked]
file internal/job/verified.go
--- anchor
		if frontier >= f {
--- replace
		if false {
--- end

[overlap lowers the frontier]
file internal/job/verified.go
--- anchor
		frontier = max(frontier, min(r.Offset+r.Length, f))
--- replace
		frontier = min(r.Offset+r.Length, f)
--- end

[the capped total's predecessor taken as uncapped]
file internal/job/cover.go
--- anchor
			if c == need {
--- replace
			if false {
--- end

[a slice past the file is not clamped to it]
file internal/job/verified.go
--- anchor
	sliceSize = min(sliceSize, fileSize)
--- replace
	sliceSize = max(sliceSize, 1)
--- end

[a block count is not capped at need, so the totals overflow]
file internal/job/cover.go
--- anchor
		blocks[i] = min(max(b, 0), need)
--- replace
		blocks[i] = max(b, 0)
--- end

[a need above MaxPar2Slices still runs the cover]
file internal/job/release.go
--- anchor
		if s.need <= MaxPar2Slices && len(idxs)*(s.need+1) <= maxCoverCells {
--- replace
		if len(idxs)*(s.need+1) <= maxCoverCells {
--- end

[the need limit excludes MaxPar2Slices itself]
file internal/job/release.go
--- anchor
		if s.need <= MaxPar2Slices && len(idxs)*(s.need+1) <= maxCoverCells {
--- replace
		if s.need < MaxPar2Slices && len(idxs)*(s.need+1) <= maxCoverCells {
--- end

[an oversized cover table still runs the cover]
file internal/job/release.go
--- anchor
		if s.need <= MaxPar2Slices && len(idxs)*(s.need+1) <= maxCoverCells {
--- replace
		if s.need <= MaxPar2Slices {
--- end

[no held volume is treated as already on disk]
file internal/job/release.go
--- anchor
		if f := &j.progress.files[i]; f.Complete && f.FailedBytes == 0 {
--- replace
		if f := &j.progress.files[i]; false && f.Complete && f.FailedBytes == 0 {
--- end

[a volume with a failed article is treated as on disk]
file internal/job/release.go
--- anchor
		if f := &j.progress.files[i]; f.Complete && f.FailedBytes == 0 {
--- replace
		if f := &j.progress.files[i]; f.Complete {
--- end

[an incomplete volume is treated as on disk]
file internal/job/release.go
--- anchor
		if f := &j.progress.files[i]; f.Complete && f.FailedBytes == 0 {
--- replace
		if f := &j.progress.files[i]; f.FailedBytes == 0 {
--- end

[the volumes on disk are not freed]
file internal/job/release.go
--- anchor
	j.undeferRecovery(onDisk)
--- replace
	_ = onDisk
--- end

[the volumes on disk stay candidates of the cover]
file internal/job/release.go
--- anchor
			onDisk = append(onDisk, i)
--- replace
			onDisk = append(onDisk, i)
			held = append(held, i)
--- end

[the volumes on disk are counted as still to fetch]
file internal/job/release.go
--- anchor
	return len(held), nil
--- replace
	return len(held) + len(onDisk), nil
--- end
