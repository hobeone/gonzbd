pkg ./internal/api/
run TestSetConfigDownloadDir_

[the application's refusal ignored by the handler]
file internal/api/config.go
--- anchor
		if err := s.downloads.SetDownloadDir(trial.General.DownloadDir); err != nil {
--- replace
		if err := s.downloads.SetDownloadDir(trial.General.DownloadDir); false {
--- end

[the raw request value passed to the application instead of the expanded one]
file internal/api/config.go
--- anchor
		if err := s.downloads.SetDownloadDir(trial.General.DownloadDir); err != nil {
--- replace
		if err := s.downloads.SetDownloadDir(value); err != nil {
--- end
