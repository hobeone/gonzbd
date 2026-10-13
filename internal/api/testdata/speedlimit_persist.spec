pkg ./internal/api/
run ^TestConfigSpeedLimit_Conversions$

[speedlimit keeps the saved bandwidth_perc]
file internal/api/config.go
--- anchor
			setErr = c.SetLocked("downloads", "bandwidth_perc", "100")
--- replace
			setErr = nil
--- end

[speedlimit does not save bandwidth_max]
file internal/api/config.go
--- anchor
			if setErr = c.SetLocked("downloads", "bandwidth_max", bsVal.String()); setErr == nil {
--- replace
			if _ = bsVal; setErr == nil {
--- end
