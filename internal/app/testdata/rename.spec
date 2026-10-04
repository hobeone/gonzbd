pkg ./internal/app/
run TestRenameJob_

[a name of only dots and spaces is accepted]
file internal/app/rename.go
--- anchor
	if strings.Trim(name, " .") == "" {
--- replace
	if strings.Trim(name, "") == "" {
--- end

[the rename is not sanitised]
file internal/app/rename.go
--- anchor
	name = fsutil.SanitizeFolderName(name, snap.Downloads.SanitizeOptions())
--- replace
	_ = fsutil.SanitizeFolderName(name, snap.Downloads.SanitizeOptions())
--- end

[the rename is not made unique]
file internal/app/rename.go
--- anchor
		name := uniqueName(base, func(n string) bool { return app.jobNameTaken(snap, n) })
--- replace
		name := base
--- end

[a name on disk counts as free]
file internal/app/rename.go
--- anchor
	if _, err := os.Lstat(filepath.Join(gen.DownloadDir, name)); err == nil {
--- replace
	if _, err := os.Lstat(filepath.Join(gen.DownloadDir, name)); false && err == nil {
--- end

[renaming to its own name renames it away]
file internal/app/rename.go
--- anchor
	if name == row.Header.Name {
		return name, nil
--- replace
	if false && name == row.Header.Name {
		return name, nil
--- end
