pkg ./internal/job/
run ^(TestNewManifestFromArticles_RefusesAnArticleCountTheTableDoesNotHave|TestAttachArticles_RefusesAManifestFromAnotherTable|TestManifestRefusesAnUnfetchableMessageIDFromDisk)$

[the article-count check is dropped from the constructor]
file internal/job/manifest.go
--- anchor
	if want := ft.TotalArticles(); len(articles) != want {
--- replace
	if want := ft.TotalArticles(); false && len(articles) != want {
--- end

[AttachArticles stops comparing the manifest's table with the job's]
file internal/job/content.go
--- anchor
	if m.Files() != j.fileTable {
--- replace
	if false {
--- end

[the Message-ID re-check is dropped from UnmarshalJSON]
file internal/job/manifest.go
--- anchor
			if !nzb.MessageIDIsFetchable(a.ID) {
--- replace
			if !nzb.MessageIDIsFetchable(a.ID) && false {
--- end
