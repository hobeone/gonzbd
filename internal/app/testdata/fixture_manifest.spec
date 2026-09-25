pkg ./internal/app/
run TestBuildTestJob_PersistsTheManifest

[the manifest writer never finds a manifest to write]
file internal/app/manifestpath.go
--- anchor
	if m, mErr := j.Manifest(); mErr == nil && m != nil {
--- replace
	if m, mErr := j.Manifest(); mErr == nil && m == nil {
--- end

[the fixture skips the write, as it did before this change]
file internal/app/testhelper_external_test.go
--- anchor
	if filepath.IsAbs(adminDir) && statErr == nil && st.IsDir() {
--- replace
	if filepath.IsAbs(adminDir) && statErr == nil && !st.IsDir() {
--- end
