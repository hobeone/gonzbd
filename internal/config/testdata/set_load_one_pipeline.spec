pkg ./internal/config/
run TestSet_ThenRestart_Issue899|TestSet_EveryAcceptedValueSurvivesRestart|TestLoadAndSetAgree

[Set validates without normalizing or expanding]
file internal/config/set.go
--- anchor
	if err := next.finish(); err != nil {
--- replace
	if err := next.Validate(); err != nil {
--- end

[Load expands and validates without normalizing]
file internal/config/loader.go
--- anchor
	if err := cfg.finish(); err != nil {
--- replace
	cfg.ExpandPaths()
	if err := cfg.Validate(); err != nil {
--- end

[finish validates before it expands]
file internal/config/loader.go
--- anchor
	c.ExpandPaths()
	return c.Validate()
--- replace
	err := c.Validate()
	c.ExpandPaths()
	return err
--- end

[log_level left unvalidated]
file internal/config/validate.go
--- anchor
	if _, err := g.ParseLogLevel(); err != nil {
--- replace
	if _, err := g.ParseLogLevel(); false && err != nil {
--- end
