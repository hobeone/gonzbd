pkg ./internal/app/
run Test(Fail_FromFetching_AbortsTheDirectUnpack|HandOver_AfterTheDownloadFinished_KeepsTheDirectUnpackResults)$

# A job handed to post-processing before its download finished has its
# DirectUnpacker aborted rather than awaited; one whose download finished
# keeps its unpacker's results.

[the hand-over waits for the unpacker of an unfinished download]
file internal/app/app.go
--- anchor
			if !downloadFinished {
				du.Abort()
			}
--- replace
			if !downloadFinished && false {
				du.Abort()
			}
--- end

[the hand-over aborts the unpacker of a finished download]
file internal/app/app.go
--- anchor
			if !downloadFinished {
				du.Abort()
			}
--- replace
			if !downloadFinished || true {
				du.Abort()
			}
--- end

[the download reads as finished whatever its files]
file internal/app/app.go
--- anchor
	downloadFinished := j.IsComplete()
--- replace
	downloadFinished := true
--- end
