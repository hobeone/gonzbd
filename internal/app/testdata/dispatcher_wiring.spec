pkg ./internal/app/
run TestApplicationConstructsAWiredDispatcher

[dispatcher is not constructed]
file internal/app/dispatcher_wiring.go
--- anchor
func (app *Application) Dispatcher() *dispatch.Dispatcher {
	return app.dispatcher
}
--- replace
func (app *Application) Dispatcher() *dispatch.Dispatcher {
	return nil
}
--- end
