pkg ./internal/unpack/
run ^(TestUnRAR_UnreadableStageIsReportedAndLogged|TestWriteEntrySafely_FailedTempRemovalIsReturned)$

# Three failures that used to vanish (#286): an unreadable unrar staging
# directory is reported with the exit status, a staging directory that
# cannot be removed is logged, and an extracted entry's temp file that cannot
# be removed is joined to the error that abandoned it.

[the unreadable stage is not reported]
file internal/unpack/unrar.go
--- anchor
			if snapErr != nil {
				res.Err = errors.Join(res.Err, fmt.Errorf("unrar: read staged files: %w", snapErr))
--- replace
			if false {
				res.Err = errors.Join(res.Err, fmt.Errorf("unrar: read staged files: %w", snapErr))
--- end

[a staging directory left behind is not logged]
file internal/unpack/snapshot.go
--- anchor
	if err := removeStageTree(stageDir); err != nil {
--- replace
	if err := removeStageTree(stageDir); false && err != nil {
--- end

[the entry's temp-file cleanup failure is discarded]
file internal/unpack/write_entry.go
--- anchor
			err = errors.Join(err, fsutil.DiscardTemp(out, func() error { return root.Remove(tmpRel) }))
--- replace
			_ = fsutil.DiscardTemp(out, func() error { return root.Remove(tmpRel) })
--- end
