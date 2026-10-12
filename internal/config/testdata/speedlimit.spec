pkg ./internal/config/
run TestDownloadConfig_SpeedLimit

[a zero percent is taken literally]
file internal/config/downloads.go
--- anchor
	if perc <= 0 || perc > 100 {
--- replace
	if perc < 0 || perc > 100 {
--- end

[a percent over 100 is taken literally]
file internal/config/downloads.go
--- anchor
	if perc <= 0 || perc > 100 {
--- replace
	if perc <= 0 {
--- end

[the percentage is not applied]
file internal/config/downloads.go
--- anchor
	return int64(c.BandwidthMax) * int64(c.EffectiveBandwidthPerc()) / 100
--- replace
	return int64(c.BandwidthMax)
--- end
