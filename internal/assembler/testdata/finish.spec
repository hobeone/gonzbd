pkg ./internal/assembler/
run Test(FileWriter_Finish|OwnedRanges_MaxEnd)

[the nothing-owned guard is dropped]
file internal/fsutil/shrink.go
--- anchor
	if end > 0 {
--- replace
	if true {
--- end

[the first fsync is skipped]
file internal/fsutil/shrink.go
--- anchor
	if err := sync(f); err != nil {
		return storagefault.Classify("sync", path, err)
	}
	if _, err := shrinkTo(f, end); err != nil {
--- replace
	if _, err := shrinkTo(f, end); err != nil {
--- end

[the second fsync is skipped]
file internal/fsutil/shrink.go
--- anchor
	if err := sync(f); err != nil {
		return storagefault.Classify("sync", path, err)
	}
	return nil
--- replace
	return nil
--- end

[the truncate is skipped]
file internal/fsutil/shrink.go
--- anchor
			if err := f.Truncate(end); err != nil {
--- replace
			if err := error(nil); err != nil {
--- end

[the truncate also grows a shorter file]
file internal/fsutil/shrink.go
--- anchor
		if fi.Size() > end {
--- replace
		if fi.Size() != end {
--- end

[finish ignores the owned end]
file internal/assembler/finish.go
--- anchor
	err := fsutil.ShrinkAndSync(w.handle, w.owned.maxEnd(), func(*os.File) error { return w.syncFile() })
--- replace
	err := fsutil.ShrinkAndSync(w.handle, 0, func(*os.File) error { return w.syncFile() })
--- end

[maxEnd reads the first range instead of the last]
file internal/assembler/ranges.go
--- anchor
	return o.s[len(o.s)-1].r.end()
--- replace
	return o.s[0].r.end()
--- end
