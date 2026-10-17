pkg ./internal/downloader/
run ^(TestDownloader_ArticleFaultingEveryTimeMovesToNextServer|TestTracker_UnmarkTriedAfterFault|TestTracker_UnmarkTriedAfterFault_PerServer)$

# An article that loses its connection maxConnFaultsPerArticle times leaves
# that server marked tried, so it moves on rather than looping there (#986).

[the per-article bound never fires]
file internal/downloader/tracker.go
--- anchor
	if perServer[serverIdx] >= maxConnFaultsPerArticle {
--- replace
	if false && perServer[serverIdx] >= maxConnFaultsPerArticle {
--- end

[one count shared by every server (CodeRabbit on #996)]
file internal/downloader/tracker.go
--- anchor
	perServer[serverIdx]++
	if perServer[serverIdx] >= maxConnFaultsPerArticle {
		delete(perServer, serverIdx)
--- replace
	perServer[0]++
	if perServer[0] >= maxConnFaultsPerArticle {
		delete(perServer, 0)
--- end

[the fetch path ignores the bound]
file internal/downloader/dispatch.go
--- anchor
	if !cancelled && isConnFault(err) {
--- replace
	if false && !cancelled && isConnFault(err) {
--- end

[a terminal article keeps its fault count]
file internal/downloader/tracker.go
--- anchor
	delete(m.tryList, key)
	delete(m.faults, key)
--- replace
	delete(m.tryList, key)
--- end

[the reaper does not drop fault counts]
file internal/downloader/tracker.go
--- anchor
		if _, ok := stale[k.inst]; ok {
			delete(m.faults, k)
--- replace
		if _, ok := stale[k.inst]; false && ok {
			delete(m.faults, k)
--- end

[Instances misses an instance holding only a fault count]
file internal/downloader/tracker.go
--- anchor
	for k := range m.faults {
		add(k)
--- replace
	for k := range m.faults {
		_ = k
--- end
