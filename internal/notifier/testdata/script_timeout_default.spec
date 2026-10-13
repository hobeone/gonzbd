pkg ./internal/notifier/
run TestNewScriptNotifier_TimeoutDefault

[the default never applied]
file internal/notifier/script.go
--- anchor
	if cfg.Timeout <= 0 {
--- replace
	if false {
--- end

[only zero takes the default]
file internal/notifier/script.go
--- anchor
	if cfg.Timeout <= 0 {
--- replace
	if cfg.Timeout == 0 {
--- end
