pkg ./internal/app/
run TestBuildStages_

[the stage reads a frozen copy of the settings]
file internal/app/stages.go
--- anchor
	unwantedStage := postproc.NewUnwantedCleanupStage(func() (unwanted.Rules, error) {
		return cfg.GetDownloads().UnwantedRules()
	})
--- replace
	frozen, frozenErr := cfg.GetDownloads().UnwantedRules()
	unwantedStage := postproc.NewUnwantedCleanupStage(func() (unwanted.Rules, error) {
		return frozen, frozenErr
	})
--- end
