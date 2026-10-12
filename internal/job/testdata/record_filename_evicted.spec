pkg ./internal/job/
run TestRecordFileFilename_RecordsWhenEvicted$

# RecordFileFilename writes the progress record alone, so it records a
# filename on an evicted job and bounds the index by the progress record's
# files (#854). Each anchor runs to the Filename write, because
# MarkFileComplete has the same bound check.

[the manifest gate restored]
file internal/job/content.go
--- anchor
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Filename = filename
--- replace
	if j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Filename = filename
--- end

[the upper bound admits one past the last file]
file internal/job/content.go
--- anchor
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Filename = filename
--- replace
	if fileIdx < 0 || fileIdx > len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Filename = filename
--- end
