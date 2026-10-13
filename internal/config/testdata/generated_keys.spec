pkg ./internal/config/
run TestLoad_GeneratesAndPersistsMissingKeys|TestLoad_KeepsAPresentKey|TestPersistGeneratedKeys_|TestSetGeneralKeys_|TestExampleConfigLoads|TestValidate_KeyErrorNamesARealRemedy

[an empty api_key is not generated]
file internal/config/keys.go
--- anchor
	if c.General.APIKey == "" {
--- replace
	if false {
--- end

[decode keeps Default's unsaved keys for a file that leaves them out]
file internal/config/loader.go
--- anchor
	cfg.General.APIKey, cfg.General.NZBKey = "", ""
--- replace
	cfg.General.Language = cfg.General.Language
--- end

[the generated keys are never written]
file internal/config/keys.go
--- anchor
	if err := writeKeysFile(g.path, out); err != nil {
--- replace
	if err := error(nil); err != nil || len(out) < 0 {
--- end

[the whole file is re-rendered through Save]
file internal/config/keys.go
--- anchor
	if err := writeKeysFile(g.path, out); err != nil {
--- replace
	if err := c.Save(g.path); err != nil || len(out) < 0 {
--- end

[a read-only file is replaced through the rename]
file internal/config/keys.go
--- anchor
	f, err := os.OpenFile(target, os.O_WRONLY, 0) //nolint:gosec // G304: operator-supplied config path
--- replace
	f, err := os.Open(target)
--- end

[a symlinked config is replaced by a regular file]
file internal/config/keys.go
--- anchor
	target, err := filepath.EvalSymlinks(path)
--- replace
	target, err := path, error(nil)
	_ = filepath.EvalSymlinks
--- end

[the file's mode is not kept]
file internal/config/keys.go
--- anchor
	if err := fsutil.WriteAtomicBytesPerm(target, out, st.Mode().Perm()); err != nil {
--- replace
	if err := fsutil.WriteAtomicBytesPerm(target, out, 0o600|st.Mode()&0); err != nil {
--- end

[a rewritten key line drops its comment]
file internal/config/keys.go
--- anchor
		if c := v.LineComment + k.LineComment; c != "" {
--- replace
		if c := ""; c != "" {
--- end

[a flow-style general mapping is not named as the reason for refusing it]
file internal/config/keys.go
--- anchor
	case gen.Kind == yaml.MappingNode && gen.Style&yaml.FlowStyle == 0:
--- replace
	case gen.Kind == yaml.MappingNode:
--- end

[the key refusal names the nonexistent gonzbd init]
file internal/config/validate.go
--- anchor
const keyRemedy = "must be 16 lowercase hex characters; generate one with `openssl rand -hex 8`, or leave it empty for gonzbd to generate one at start"
--- replace
const keyRemedy = "must be 16 lowercase hex characters; regenerate via `gonzbd init`"
--- end
