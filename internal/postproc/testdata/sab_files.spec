pkg ./internal/postproc/
run TestScriptStage_SABFiles|TestBuildEnv_FilesAlwaysValidJSONArray

[the sort of the listed paths dropped]
file internal/postproc/stage_script.go
--- anchor
	slices.Sort(files)
--- replace
	_ = slices.Sort[[]string]
--- end

[paths listed absolute instead of relative to the job directory]
file internal/postproc/stage_script.go
--- anchor
		files = append(files, filepath.ToSlash(rel))
--- replace
		_ = rel
		files = append(files, filepath.ToSlash(path))
--- end

[the walk error swallowed]
file internal/postproc/stage_script.go
--- anchor
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
--- replace
		if err != nil {
			return nil
		}
		if !d.Type().IsRegular() {
--- end

[an empty list encoded as null]
file internal/postproc/script.go
--- anchor
	if len(files) == 0 {
		return "[]"
	}
--- replace
--- end

[directories and symlinks listed with the files]
file internal/postproc/stage_script.go
--- anchor
		if !d.Type().IsRegular() {
--- replace
		if false {
--- end

[the encoded-size bound dropped]
file internal/postproc/stage_script.go
--- anchor
	if err == nil && len(encodeFiles(files)) > maxFilesEnvBytes {
--- replace
	if false {
--- end
