pkg ./internal/postproc/
run ^TestRemovePendingSidecar_LogsOnlyARealFailure$

# A pending-deletions sidecar that could not be removed is logged at Warn,
# one already gone is not, and a nil logger is tolerated (#286).

[a refused removal is discarded]
file internal/postproc/stage_finalize.go
--- anchor
	if err != nil && !errors.Is(err, os.ErrNotExist) && log != nil {
--- replace
	if false && err != nil && !errors.Is(err, os.ErrNotExist) && log != nil {
--- end

[a sidecar already gone is logged]
file internal/postproc/stage_finalize.go
--- anchor
	if err != nil && !errors.Is(err, os.ErrNotExist) && log != nil {
--- replace
	if err != nil && log != nil {
--- end

[a nil logger is used]
file internal/postproc/stage_finalize.go
--- anchor
	if err != nil && !errors.Is(err, os.ErrNotExist) && log != nil {
--- replace
	if err != nil && !errors.Is(err, os.ErrNotExist) {
--- end
