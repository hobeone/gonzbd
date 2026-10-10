pkg ./internal/job/
run ^(TestManifestArticleInFile|TestMarkArticleWritten_AcceptsAZeroLengthRow|TestMarkArticleWritten_RejectsAnInvalidShape|TestMarkArticleWritten_MarksDoneAndKeepsTheRow|TestPlaceRows_KeepsOnlyRowsOfTheFile|TestInstallVerified_ARowItCannotPlaceCostsOnlyItself)$

# One range predicate, Manifest.ArticleInFile, and one shape predicate,
# WrittenRow.HasValidShape, guard every row that resolves an article, live and
# at a restart. A zero-length article is valid at both.

[the predicate ignores the file's upper bound]
file internal/job/manifest.go
--- anchor
	return int(artIdx) >= lo && int(artIdx) < hi
--- replace
	return int(artIdx) >= lo && hi >= 0
--- end

[placeRows drops the shared predicate]
file internal/job/verified.go
--- anchor
		if r.FileIdx != fileIdx || !m.ArticleInFile(r.FileIdx, r.ArtIdx) || !r.HasValidShape() {
--- replace
		if r.FileIdx != fileIdx || !r.HasValidShape() {
--- end

[MarkArticleWritten drops the shared predicate]
file internal/job/verified.go
--- anchor
	if !m.ArticleInFile(row.FileIdx, row.ArtIdx) {
--- replace
	if row.FileIdx < 0 || row.FileIdx >= m.NumFiles() {
--- end

[MarkArticleWritten rejects a zero-length row]
file internal/job/verified.go
--- anchor
	if !m.ArticleInFile(row.FileIdx, row.ArtIdx) {
--- replace
	if !m.ArticleInFile(row.FileIdx, row.ArtIdx) || row.Length <= 0 {
--- end

[MarkArticleWritten drops the shape predicate]
file internal/job/verified.go
--- anchor
	if !row.HasValidShape() {
--- replace
	if false {
--- end
