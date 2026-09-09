pkg ./internal/app/
run TestUnpackConfigFromPP_ForwardsProbeHasProblem

[the probe's HasProblem never reaches the unpack config]
file internal/app/reload_translate.go
--- anchor
			HasProblem:       probe.UnrarInfo.HasProblem,
--- replace
			HasProblem:       false,
--- end
