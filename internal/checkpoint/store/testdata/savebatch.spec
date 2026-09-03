pkg ./internal/checkpoint/store/
run TestSaveBatch

[the missing-row refusal neutered]
file internal/checkpoint/store/store.go
--- anchor
		if n == 0 {
--- replace
		if n < 0 {
--- end

[a per-job failure no longer aborts the batch]
file internal/checkpoint/store/store.go
--- anchor
		if err := saveOne(ctx, stmt, cp); err != nil {
--- replace
		if err := saveOne(ctx, stmt, cp); false {
--- end

[the Complete flag written as its opposite]
file internal/checkpoint/store/store.go
--- anchor
			p.FileComplete(fi), int(p.FileFetchPolicy(fi)), p.FileFilename(fi),
--- replace
			!p.FileComplete(fi), int(p.FileFetchPolicy(fi)), p.FileFilename(fi),
--- end
