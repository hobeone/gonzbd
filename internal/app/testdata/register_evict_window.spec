pkg ./internal/app/
run TestRegisterFile_EvictedAfterCountRecordsFilename$

# An eviction between registerFile's count of the file's unfinished articles
# and its recording of the file's filename must still record the filename,
# which the dirty mark then persists (#854). The hooked test evicts in that
# window.

[RecordFileFilename refuses an evicted job again]
file internal/job/content.go
--- anchor
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
--- replace
	if j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
--- end

[the dirty mark skipped]
file internal/app/pipeline.go
--- anchor
		if p.markDirty != nil {
			p.markDirty(j, fileIdx)
		}
	}
	p.fileInfo[key] = info
--- replace
	}
	p.fileInfo[key] = info
--- end
