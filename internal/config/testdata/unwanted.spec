pkg ./internal/config/
run TestValidateDownloads_UnwantedExtensions|TestSet_UnwantedExtensionsEnumsValidated|TestDefault_UnwantedExtensions

[validation does not check the unwanted-extension settings]
file internal/config/validate.go
--- anchor
	if _, err := d.UnwantedRules(); err != nil {
--- replace
	if _, err := d.UnwantedRules(); false && err != nil {
--- end

[GetDownloads shares the list]
file internal/config/config.go
--- anchor
	dl.UnwantedExtensions = slices.Clone(c.Downloads.UnwantedExtensions)
	return dl
--- replace
	return dl
--- end

[IngestSnapshot shares the list]
file internal/config/config.go
--- anchor
	dl.UnwantedExtensions = slices.Clone(c.Downloads.UnwantedExtensions)
	return IngestSnapshot{
--- replace
	return IngestSnapshot{
--- end

[Snapshot shares the list]
file internal/config/config.go
--- anchor
	res.Downloads.UnwantedExtensions = slices.Clone(c.Downloads.UnwantedExtensions)
--- replace
--- end
