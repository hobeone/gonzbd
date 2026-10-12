pkg ./internal/web/
run TestHandlerFrom_ReturnsSubError

[handlerFrom drops the fs.Sub error]
file internal/web/server.go
--- anchor
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
--- replace
	dist, err := fs.Sub(embedded, "dist")
	if false && err != nil {
--- end
