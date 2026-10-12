pkg ./internal/job/
run TestMarkFileComplete_MarksWhenEvicted$

# MarkFileComplete writes the progress record alone, so it marks a file of an
# evicted job complete and bounds the index by the progress record's files
# (#868). Each anchor runs to the Complete write, because RecordFileFilename
# has the same bound check.

[the manifest gate restored]
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

[the upper bound admits one past the last file]
file internal/job/content.go
--- anchor
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Complete = true
--- replace
	if fileIdx < 0 || fileIdx > len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Complete = true
--- end
