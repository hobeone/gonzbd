# Strict server-priority tiers (#985): an article is offered to a priority
# only once every server of a better priority has tried it or is not active,
# and a pass stops once no unvisited article can be dispatched.
pkg ./internal/downloader/
run TestTryDispatch_FullPrimaryHoldsTheArticleRatherThanOverflowing|TestTryDispatch_AnArticleThePrimaryTriedGoesToTheBackup|TestTryDispatch_AWaitingArticleMovesDownWhenThePrimaryIsPenalized|TestTryDispatch_PriorityOutranksConfigOrder|TestTryDispatch_AFullMiddleTierHoldsTheArticleWhenTheTopIsPenalized|TestTryDispatch_ServersSharingATierBalanceBelowAPenalizedTop|TestTryDispatch_WorkChFullSkipsToNextServer|TestBuildDispatchPlan_AnArticleNeedingTheBackupIsNotStuckBehindWaitingOnes|TestBuildDispatchPlan_StopsOnceNoUnvisitedArticleCanBeDispatched|TestBuildDispatchPlan_ReportsEveryArticleWithNoServerLeft|TestDownloader_BackupStaysIdleWhileThePrimaryHasEveryArticle|TestDownloader_OnlyArticlesThePrimaryLacksReachTheBackup|TestDownloader_WorkMovesToTheBackupWhenThePrimaryIsPenalized

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

[the pass never counts a visited try-list entry]
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

[the pass starts believing no entry is left to visit]
file internal/downloader/dispatch.go
--- anchor
	triedLeft, _ := d.tracker.Len()
--- replace
	triedLeft := 0
--- end

[the pass never stops on a waiting fresh article]
file internal/downloader/dispatch.go
--- anchor
	if triedLeft <= 0 {
--- replace
	if false {
--- end

[an exhausted article stops the pass]
file internal/downloader/dispatch.go
--- anchor
		return srv == nil && idx != -1
--- replace
		return srv == nil && idx == idx
--- end
