pkg ./internal/assembler/
run TestOnArticleWritten|TestFileInfoOwned|TestOpenTargetFile_

[the callback call neutered]
file internal/assembler/assembler.go
--- anchor
	if a.opts.OnArticleWritten != nil {
--- replace
	if false {
--- end

[the seed loop neutered]
file internal/assembler/assembler.go
--- anchor
	if err := w.owned.seed(valid); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end

[range validation neutered so an invalid range is seeded]
file internal/assembler/assembler.go
--- anchor
		if r.Off < 0 || r.Len < 0 || r.Off > math.MaxInt64-r.Len {
--- replace
		if false {
--- end

[the callback is skipped for a zero-length article]
file internal/assembler/assembler.go
--- anchor
	if a.opts.OnArticleWritten != nil {
--- replace
	if a.opts.OnArticleWritten != nil && o.n > 0 {
--- end

[a faulted write is resolved Written, so the callback fires for it]
file internal/assembler/assembler.go
--- anchor
		return outcome{disposition: dispositionRolledBack, fault: err}
--- replace
		return outcome{disposition: dispositionWritten, n: n}
--- end
