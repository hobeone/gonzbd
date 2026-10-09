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
	if err := w.Sync(); err != nil {
		return fmt.Errorf("finish %s: first fsync: %w", w.path, err)
	}
--- replace
--- end

[the second fsync is skipped]
file internal/assembler/finish.go
--- anchor
	if err := w.Sync(); err != nil {
		return fmt.Errorf("finish %s: second fsync: %w", w.path, err)
	}
--- replace
--- end

[the truncate is skipped]
file internal/assembler/finish.go
--- anchor
		if err := w.Truncate(end); err != nil {
--- replace
		if err := error(nil); err != nil {
--- end

[the truncate also grows a shorter file]
file internal/assembler/filewriter.go
--- anchor
	if n >= fi.Size() {
--- replace
	if n == fi.Size() {
--- end

[maxEnd reads the first range instead of the last]
file internal/assembler/ranges.go
--- anchor
	return o.s[len(o.s)-1].r.end()
--- replace
	return o.s[0].r.end()
--- end
