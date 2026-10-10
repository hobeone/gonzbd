pkg ./internal/app/
run TestEnqueuePostProc_DeliveredCrashRecovery

[enqueuePostProc leaves FlatLayout false on flat category]
file internal/app/app.go
--- anchor
			DownloadDir:          downloadDir,
			FinalDir:             finalDir,
			FlatLayout:           flatLayout,
--- replace
			DownloadDir:          downloadDir,
			FinalDir:             finalDir,
			FlatLayout:           false && flatLayout,
--- end

[enqueuePostProc sets FlatLayout true on per-job category]
file internal/app/app.go
--- anchor
			DownloadDir:          downloadDir,
			FinalDir:             finalDir,
			FlatLayout:           flatLayout,
--- replace
			DownloadDir:          downloadDir,
			FinalDir:             finalDir,
			FlatLayout:           true || flatLayout,
--- end
