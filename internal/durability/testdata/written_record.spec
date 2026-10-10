pkg ./internal/durability/
run TestApplyRecord_
timeout 3m

[the EXISTS guard no longer requires a job_files row]
file internal/durability/written.go
--- anchor
	if !live {
--- replace
	if false {
--- end

[DeleteAll deletes nothing]
file internal/durability/written.go
--- anchor
			`DELETE FROM written_articles WHERE job_id = ? AND file_idx = ?`, jobID, v.FileIdx); err != nil {
--- replace
			`DELETE FROM written_articles WHERE 0 AND job_id = ? AND file_idx = ?`, jobID, v.FileIdx); err != nil {
--- end

[DeleteArtIdxs deletes nothing]
file internal/durability/written.go
--- anchor
			`DELETE FROM written_articles WHERE job_id = ? AND file_idx = ? AND art_idx = ?`, jobID, v.FileIdx, a); err != nil {
--- replace
			`DELETE FROM written_articles WHERE 0 AND job_id = ? AND file_idx = ? AND art_idx = ?`, jobID, v.FileIdx, a); err != nil {
--- end

[SetComplete with ClearComplete is no longer rejected]
file internal/durability/written.go
--- anchor
		if v.SetComplete && v.ClearComplete {
--- replace
		if false {
--- end
