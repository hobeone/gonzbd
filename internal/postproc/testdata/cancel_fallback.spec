pkg ./internal/postproc/
run TestDispatchRepairTool_CancelledContextSkipsFallback|TestExtractWithDriver_CancelledContextSkipsFallback

[repair fallback ignores cancellation]
file internal/postproc/stage_repair.go
--- anchor
	if fallback && ctx.Err() == nil && shouldFallbackToExternal(res, err) {
--- replace
	if fallback && shouldFallbackToExternal(res, err) {
--- end

[unpack fallback ignores cancellation]
file internal/postproc/stage_unpack.go
--- anchor
	if goErr != nil && ctx.Err() == nil && d.fallback {
--- replace
	if goErr != nil && d.fallback {
--- end
