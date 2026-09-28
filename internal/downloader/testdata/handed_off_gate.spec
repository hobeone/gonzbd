pkg ./internal/downloader/
run Test(BuildDispatchPlan_SkipsAHandedOffJob|HasDownloadableJobs_SkipsAHandedOffJob)$

# Options.HandedOff keeps a job handed to post-processing out of dispatch and
# out of the downloadable count, whatever its row says.

[buildDispatchPlan ignores the hand-off]
file internal/downloader/dispatch.go
--- anchor
		if !ok || !j.Resident() || d.handedOff(j) {
--- replace
		if !ok || !j.Resident() {
--- end

[hasDownloadableJobs ignores the hand-off]
file internal/downloader/dispatch.go
--- anchor
		if j, ok := d.dispatcher.Job(row.ID); ok && d.handedOff(j) {
--- replace
		if false {
--- end

[handedOff never consults the option]
file internal/downloader/dispatch.go
--- anchor
	return d.opts.HandedOff != nil && d.opts.HandedOff(j)
--- replace
	return false
--- end
