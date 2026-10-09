pkg ./internal/assembler/
run TestOnArticleWritten|TestFileInfoOwned|TestOpenTargetFile_Seeds

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
		if r.Off < 0 || r.Len <= 0 || r.Off > math.MaxInt64-r.Len {
--- replace
		if false {
--- end

[the callback fires even when the write faulted]
file internal/assembler/assembler.go
--- anchor
	if err := f.w.Accept(id, req.Offset, req.Data, req.CRC32); err != nil {
--- replace
	if err := f.w.Accept(id, req.Offset, req.Data, req.CRC32); err != nil && false {
--- end
