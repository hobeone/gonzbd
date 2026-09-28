pkg ./internal/downloader/
run TestDecodePayload_UUMultipartRejected|TestDecodePayload_UUSinglePartStillDecodes|TestProcessFetchedArticle_UUMultipartIsTerminal

[E5 guard neutered: a UU decode for segment > 1 claims offset 0 instead of being rejected — the shipped #346 bug]
file internal/downloader/dispatch.go
--- anchor
			if requestedPartNumber > 1 {
--- replace
			if false {
--- end
