pkg ./internal/app/
run TestBuildNotifier_SinksCarryConfig

[email sink never registered]
file internal/app/notify.go
--- anchor
	if cfg.Email.Enable {
--- replace
	if false {
--- end

[apprise sink never registered]
file internal/app/notify.go
--- anchor
	if cfg.Apprise.Enable {
--- replace
	if false {
--- end

[script sink never registered]
file internal/app/notify.go
--- anchor
	if cfg.Script.Enable {
--- replace
	if false {
--- end

[apprise mask taken from email events]
file internal/app/notify.go
--- anchor
			EventMask:  parseEventMask(cfg.Apprise.Events),
--- replace
			EventMask:  parseEventMask(cfg.Email.Events),
--- end

[script mask taken from email events]
file internal/app/notify.go
--- anchor
			EventMask: parseEventMask(cfg.Script.Events),
--- replace
			EventMask: parseEventMask(cfg.Email.Events),
--- end
