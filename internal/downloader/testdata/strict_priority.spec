# Strict server-priority tiers (#985): an article is offered to a priority
# only once every server of a better priority has tried it or is not active,
# and a pass stops walking a job once nothing unvisited in it can dispatch.
pkg ./internal/downloader/
run TestTryDispatch_FullPrimaryHoldsTheArticleRatherThanOverflowing|TestTryDispatch_AnArticleThePrimaryTriedGoesToTheBackup|TestTryDispatch_AWaitingArticleMovesDownWhenThePrimaryIsPenalized|TestTryDispatch_PriorityOutranksConfigOrder|TestTryDispatch_AFullMiddleTierHoldsTheArticleWhenTheTopIsPenalized|TestTryDispatch_ServersSharingATierBalanceBelowAPenalizedTop|TestTryDispatch_TheFirstPeerWithRoomTakesTheArticle|TestTryDispatch_WorkChFullSkipsToNextServer|TestBuildDispatchPlan_AnArticleNeedingTheBackupIsNotStuckBehindWaitingOnes|TestBuildDispatchPlan_StopsOnceNoUnvisitedArticleCanBeDispatched|TestBuildDispatchPlan_AWaitingJobDoesNotHideALaterJobsBackupArticle|TestBuildDispatchPlan_ReportsEveryArticleWithNoServerLeft|TestDownloader_BackupStaysIdleWhileThePrimaryHasEveryArticle|TestDownloader_OnlyArticlesThePrimaryLacksReachTheBackup|TestDownloader_WorkMovesToTheBackupWhenThePrimaryIsPenalized

[a worse tier is no longer skipped, so a full tier overflows]
file internal/downloader/dispatch.go
--- anchor
		if tierFound && cfg.Priority > tier {
--- replace
		if false {
--- end

[a server sharing the tier is skipped like a worse one]
file internal/downloader/dispatch.go
--- anchor
		if tierFound && cfg.Priority > tier {
--- replace
		if tierFound && cfg.Priority >= tier {
--- end

[an inactive candidate holds the article back]
file internal/downloader/dispatch.go
--- anchor
		anyCandidate = true
		if !srv.Active(opts.now) {
--- replace
		anyCandidate = true
		if false {
--- end

[a better tier found later keeps the worse tier's pick]
file internal/downloader/dispatch.go
--- anchor
			pick, pickIdx = nil, -2
--- replace
			tier += 0
--- end

[the last peer with room, not the first, takes the article]
file internal/downloader/dispatch.go
--- anchor
		if pick == nil && hasRoom(d.workCh[cfg.Name]) {
--- replace
		if hasRoom(d.workCh[cfg.Name]) {
--- end

[the walk never counts a visited try-list entry]
file internal/downloader/dispatch.go
--- anchor
			if hadTryList {
--- replace
			if hadTryList && false {
--- end

[an in-flight article's entry is not reported as visited]
file internal/downloader/dispatch.go
--- anchor
	if d.tracker.InFlightLocked(key) > 0 {
		d.tracker.Unlock()
		return false, nil, hasTried
--- replace
	if d.tracker.InFlightLocked(key) > 0 {
		d.tracker.Unlock()
		return false, nil, false
--- end

[a fresh dispatch is reported as a visited entry]
file internal/downloader/dispatch.go
--- anchor
				d.tracker.Unlock()
				return true, nil, hasTried
--- replace
				d.tracker.Unlock()
				return true, nil, true
--- end

[a job's several entries are counted as one]
file internal/downloader/tracker.go
--- anchor
		out[k.inst]++
--- replace
		out[k.inst] = 1
--- end

[the walk does not stop once every server is full]
file internal/downloader/dispatch.go
--- anchor
			if triedLeft > 0 || !d.freshArticlesWait(opts) {
				if d.allServersFull(opts.serverCfgs) {
--- replace
			if triedLeft > 0 || !d.freshArticlesWait(opts) {
				if false {
--- end

[the count is not scoped to the walked job]
file internal/downloader/dispatch.go
--- anchor
		triedLeft := tried[weak.Make(j)]
--- replace
		triedLeft := len(tried) + 0*tried[weak.Make(j)]
--- end

[a job with nothing to dispatch is walked anyway]
file internal/downloader/dispatch.go
--- anchor
		if triedLeft == 0 && d.freshArticlesWait(opts) {
--- replace
		if false {
--- end

[the walk never stops on a waiting fresh article]
file internal/downloader/dispatch.go
--- anchor
			if triedLeft > 0 || !d.freshArticlesWait(opts) {
--- replace
			if true {
--- end

[a job's walk ending ends the whole pass]
file internal/downloader/dispatch.go
--- anchor
			stopAll = d.allServersFull(opts.serverCfgs)
			return false
--- replace
			stopAll = true
			return false
--- end

[an exhausted article counts as waiting]
file internal/downloader/dispatch.go
--- anchor
	return srv == nil && idx != -1
--- replace
	return srv == nil && idx == idx
--- end
