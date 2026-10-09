pkg ./internal/assembler/
run Test(FileWriter_Finish|OwnedRanges_MaxEnd)

[the nothing-owned guard is dropped]
file internal/assembler/finish.go
--- anchor
	if end := w.owned.maxEnd(); end > 0 {
--- replace
	if end := w.owned.maxEnd(); true {
--- end

[the first fsync is skipped]
file internal/assembler/finish.go
--- anchor
	if err := w.syncFile(); err != nil {
		return fmt.Errorf("finish %s: first fsync: %w", w.path, err)
	}
--- replace
--- end

[the truncate also grows a shorter file]
file internal/assembler/finish.go
--- anchor
		if fi.Size() > end {
--- replace
		if fi.Size() != end {
--- end

[maxEnd reads the first range instead of the last]
file internal/assembler/ranges.go
--- anchor
	return o.s[len(o.s)-1].r.end()
--- replace
	return o.s[0].r.end()
--- end
