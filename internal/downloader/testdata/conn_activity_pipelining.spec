pkg ./internal/downloader/
run TestConnActivity_PipelinedSlotBusyUntilLastArticle

[the whole slot reset instead of the one completed article — the shipped bug]
file internal/downloader/downloader.go
--- anchor
				ca.inflight = slices.Delete(ca.inflight, i, i+1)
--- replace
				ca.inflight = slices.Delete(ca.inflight, 0, len(ca.inflight))
--- end

[the slot reports its newest article instead of its oldest]
file internal/downloader/downloader.go
--- anchor
	return ca.inflight[0], true
--- replace
	return ca.inflight[len(ca.inflight)-1], true
--- end

[the slot reports one article rather than its true pipelined depth]
file internal/downloader/downloader.go
--- anchor
			snap.InFlight = len(ca.inflight)
--- replace
			snap.InFlight = 1
--- end
