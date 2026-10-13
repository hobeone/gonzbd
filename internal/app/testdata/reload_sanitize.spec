pkg ./internal/app/
run TestReloadDownloadOptions_NamesNewFilesWithTheNewSanitizeOptions

[the reload leaves the pipeline's sanitize options at their startup value]
file internal/app/reloader.go
--- anchor
	app.pipeline.setSanitize(d.SanitizeOptions())
--- replace
	_ = d.SanitizeOptions
--- end

[the setter does not store the options]
file internal/app/pipeline.go
--- anchor
	p.sanitize = o
--- replace
	_ = o
--- end
