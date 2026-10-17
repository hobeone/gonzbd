pkg ./scripts/localnntp/
run TestNewServer_ReleaseDirChecks|TestServer_MultiRelease|TestServer_ArticlesPack_FastPath

[any open error on articles.pack falls back to the slow path]
run TestNewServer_ReleaseDirChecks
file scripts/localnntp/server.go
--- anchor
	if errors.Is(err, fs.ErrNotExist) {
--- replace
	if errors.Is(err, fs.ErrNotExist) || err != nil {
--- end

[a pack that is not a regular file is accepted]
run TestNewServer_ReleaseDirChecks
file scripts/localnntp/server.go
--- anchor
	if err == nil && !fi.Mode().IsRegular() {
--- replace
	if false {
--- end

[a pack shorter than the manifest needs is accepted]
run TestNewServer_ReleaseDirChecks
file scripts/localnntp/server.go
--- anchor
	if err == nil && fi.Size() < packEnd {
--- replace
	if false {
--- end

[a release without a files directory is accepted]
run TestNewServer_ReleaseDirChecks
file scripts/localnntp/server.go
--- anchor
		if fi, err := os.Stat(filepath.Join(src.Dir, "files")); err != nil || !fi.IsDir() {
--- replace
		if fi, err := os.Stat(filepath.Join(src.Dir, "files")); false && (err != nil || !fi.IsDir()) {
--- end

[the fast path reads the first release's pack for every release]
run TestServer_MultiRelease_FastPathUsesOwnPack
file scripts/localnntp/server.go
--- anchor
	if _, err := s.releases[loc.Release].packFile.ReadAt(buf, loc.WireOffset); err != nil {
--- replace
	if _, err := s.releases[0].packFile.ReadAt(buf, loc.WireOffset); err != nil {
--- end

[the fast-path check consults the first release's pack for every release]
run TestServer_MultiRelease_FastPathIsPerRelease
file scripts/localnntp/server.go
--- anchor
	if !hasFault && s.releases[loc.Release].packFile != nil && loc.WireLength > 0 {
--- replace
	if !hasFault && s.releases[0].packFile != nil && loc.WireLength > 0 {
--- end

[the slow path reads the first release's files directory for every release]
run TestServer_MultiRelease$
file scripts/localnntp/server.go
--- anchor
	filePath := filepath.Join(s.releases[loc.Release].dir, "files", loc.DiskName)
--- replace
	filePath := filepath.Join(s.releases[0].dir, "files", loc.DiskName)
--- end
