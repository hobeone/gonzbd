pkg ./internal/fsutil/
run ^(TestWriteAtomic_FailedTempRemovalIsReturned|TestWriteAtomic_CleanupReportsNoFalseFailure)$

# writeAtomic's cleanup joins a failure to remove the unpublished temp file
# to the error it returns, and reports neither a close of a temp file the
# success path already closed nor a removal of one already gone (#286).

[the cleanup failure is discarded]
file internal/fsutil/atomic.go
--- anchor
			err = errors.Join(err, DiscardTemp(tmp, func() error { return os.Remove(tmpName) }))
--- replace
			_ = DiscardTemp(tmp, func() error { return os.Remove(tmpName) })
--- end

[a close of an already-closed temp file is reported]
file internal/fsutil/atomic.go
--- anchor
	if errors.Is(cerr, os.ErrClosed) {
--- replace
	if false {
--- end

[a removal of an already-gone temp file is reported]
file internal/fsutil/atomic.go
--- anchor
	if errors.Is(rerr, os.ErrNotExist) {
--- replace
	if false {
--- end
