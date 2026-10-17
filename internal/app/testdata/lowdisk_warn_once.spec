pkg ./internal/app/
run TestHandleLowDisk_WarnsOncePerTransition$

[every low-disk callback is a transition]
file internal/app/app.go
--- anchor
	transition := app.pauseReason == pauseReasonNone
--- replace
	transition := true
--- end

[the warning level ignores the transition]
file internal/app/app.go
--- anchor
	if transition {
		level = slog.LevelWarn
	}
--- replace
	if transition || !transition {
		level = slog.LevelWarn
	}
--- end
