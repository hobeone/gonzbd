pkg ./internal/app/
run TestCompleteFinalizedFile_Evicted

# A completion whose job is evicted after the residency check is marked,
# recorded and reported, because MarkFileComplete needs only progress; one
# whose job was evicted before it is not marked, because the settle, the peek
# and the DirectUnpack feed may have been skipped (#868).

[MarkFileComplete refuses an evicted job again]
file internal/job/content.go
--- anchor
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Complete = true
--- replace
	if j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Complete = true
--- end

[the residency check before the mark dropped]
file internal/app/app.go
--- anchor
			if !j.Resident() {
--- replace
			if false {
--- end
