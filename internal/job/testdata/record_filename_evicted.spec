pkg ./internal/job/
run TestRecordFileFilename_RecordsWhenEvicted$

# RecordFileFilename writes the progress record alone, so it records a
# filename on an evicted job and bounds the index by the progress record's
# files (#854).

[the manifest gate restored]
file internal/job/content.go
--- anchor
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
--- replace
	if j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
--- end

[the upper bound admits one past the last file]
file internal/job/content.go
--- anchor
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
--- replace
	if fileIdx < 0 || fileIdx > len(j.progress.files) {
--- end
