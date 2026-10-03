pkg ./internal/app/
run TestManifestPath_HasOneProductionCaller|TestRetryHistoryJob_UnwritableManifestDirLeavesTheEntryRetryable

[the retry builds a manifest path of its own again]
file internal/app/app.go
--- anchor
	if err := writeJobManifest(app.config.GetGeneral().AdminDir, j); err != nil {
		return fmt.Errorf("app: retry %s: %w", jobID, err)
--- replace
	if err := writeJobManifest(app.config.GetGeneral().AdminDir, j); err != nil {
		_, _ = manifestPath("", jobID); return fmt.Errorf("app: retry %s: %w", jobID, err)
--- end

[the owner swallows a failed mkdir]
file internal/app/manifestpath.go
--- anchor
		return fmt.Errorf("mkdir manifests: %w", err)
--- replace
		_ = err
--- end
