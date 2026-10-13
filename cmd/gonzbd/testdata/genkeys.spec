pkg ./cmd/gonzbd/
run TestLoadOrCreateConfig_PersistsGeneratedKeys$

[serve mode does not write the generated keys back]
file cmd/gonzbd/main.go
--- anchor
		if persist {
			if err := cfg.PersistGeneratedKeys(); err != nil {
--- replace
		if false {
			if err := cfg.PersistGeneratedKeys(); err != nil {
--- end

[one-shot mode writes them too]
file cmd/gonzbd/main.go
--- anchor
		if persist {
			if err := cfg.PersistGeneratedKeys(); err != nil {
--- replace
		if true {
			if err := cfg.PersistGeneratedKeys(); err != nil {
--- end
