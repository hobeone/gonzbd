pkg ./internal/app/
run ^TestLooseRecord_RetryShapeMismatchDeletesEveryRow$

# A retry deletes every row when one does not fit the re-parsed manifest, by
# the shared range predicate.

[rowsFitManifest drops the shared predicate]
file internal/app/app.go
--- anchor
		if !m.ArticleInFile(r.FileIdx, r.ArtIdx) {
--- replace
		if r.FileIdx < 0 || r.FileIdx >= m.NumFiles() {
--- end
