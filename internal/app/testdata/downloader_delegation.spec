pkg ./internal/app/
run ^TestServerControls_DelegateToTheDownloader$

# The server controls reach the downloader New installed.

[DisconnectAll drops the call]
file internal/app/app.go
--- anchor
	app.downloader.DisconnectAll()
--- replace
	_ = app.downloader
--- end

[UnblockServer answers without asking the downloader]
file internal/app/app.go
--- anchor
	return app.downloader.UnblockServer(name)
--- replace
	return name != ""
--- end
