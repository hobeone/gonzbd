pkg ./internal/downloader/
run TestDecodePayload_UUPartGreaterThanOneRejected|TestDecodePayload_UUFirstSegmentStillDecodes|TestDecodePayload_YencNoYPartRejectedForNonFirstSegment|TestDecodePayload_YencNoYPartStillDecodesForFirstSegment|TestProcessFetchedArticle_OffsetUnknownForPartIsTerminal

[E5 guard neutered on the UU branch: a UU decode for segment > 1 claims offset 0 instead of being rejected — the shipped #346 bug]
file internal/downloader/dispatch.go
--- anchor
			if requestedPartNumber > 1 {
--- replace
			if false {
--- end

[E5 guard neutered on the yEnc branch: a yEnc body with no =ypart line claims offset 0 for segment > 1 instead of being rejected — the same #346 bug from the other decoder]
file internal/downloader/dispatch.go
--- anchor
		if !article.HasOffset && requestedPartNumber > 1 {
--- replace
		if false {
--- end
