pkg ./internal/downloader/
run TestDownloader_SlowWriterCapsHeldBytesAtTheBudget|TestDownloader_StopUnblocksFetchesWaitingOnTheBudget|TestDownloader_AFetchThatYieldsNoBodyGivesBackItsCharge|TestProcessFetchedArticle_ReleasesTheHoldOfAFailedDecode|TestProcessFetchedArticle_HandsTheHoldOnWithTheResult|TestEmitResult_ADroppedResultReleasesItsHold

[fetches are not charged to the budget, so a slow writer no longer stops them]
file internal/downloader/dispatch.go
--- anchor
	held, err := d.held.Acquire(ctx, decoder.BufferCap(int64(req.bytes)))
--- replace
	held, err := d.held.Acquire(ctx, 0)
--- end

[a fetch that yields no body keeps its charge]
file internal/downloader/dispatch.go
--- anchor
	if !ok {
		held.Release()
--- replace
	if !ok {
		_ = held
--- end

[a failed decode keeps its hold]
file internal/downloader/dispatch.go
--- anchor
	if err != nil {
		held.Release()
--- replace
	if err != nil {
		_ = held
--- end

[a result dropped at shutdown keeps its hold]
file internal/downloader/dispatch.go
--- anchor
	case <-ctx.Done():
		res.Release()
--- replace
	case <-ctx.Done():
		_ = res
--- end

[the success path drops the hold instead of handing it on]
file internal/downloader/dispatch.go
--- anchor
		Held:       held,
--- replace
		Held:       nil,
--- end
