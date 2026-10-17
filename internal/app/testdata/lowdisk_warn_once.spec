pkg ./internal/app/
run TestHandleLowDisk_WarnsOncePerTransition$

[every low-disk callback is a transition]
file internal/app/app.go
--- anchor
	transition := !app.lowDiskWarned
--- replace
	transition := true
--- end

[resume does not re-arm the warning]
file internal/app/app.go
--- anchor
	app.lowDiskWarned = false
--- replace
	_ = app.lowDiskWarned
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
