pkg ./internal/job/
run ^(TestMarkArticleWritten_ReplacesARowAResetLeftBehind|TestMarkArticleWritten_ReplacesWithoutReachingAClone|TestMarkArticleWritten_MarksDoneAndKeepsTheRow|TestInstallVerified_ReplacesAnArticlesEarlierRow|TestInstallVerified_MergesALaterInstallWithTheResidentRows|TestInstallRows_KeepsACopyOfTheFirstInstall|TestFileRows_ReturnsACopy)$

# upsertRows is the one replace-or-append of resident rows: a row for an
# article that has one replaces it, in a new slice.

[an existing row is duplicated, not replaced]
file internal/job/verified.go
--- anchor
		k, ok := at[r.ArtIdx]
--- replace
		k, ok := at[r.ArtIdx]
		ok = ok && false
--- end

[a replacement edits the stored slice in place]
file internal/job/verified.go
--- anchor
		if !copied {
			out = slices.Clone(out)
			copied = true
		}
--- replace
		if !copied {
			copied = true
		}
--- end
