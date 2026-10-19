pkg ./internal/postproc/
run TestScriptStage_DownloadFigures

[downloaded bytes left at zero, as before #1014]
file internal/postproc/stage_script.go
--- anchor
		BytesDownloaded: downloaded,
--- replace
		BytesDownloaded: 0,
--- end

[bytes tried left at zero]
file internal/postproc/stage_script.go
--- anchor
		BytesTried:      expectedBytes,
--- replace
		BytesTried:      0,
--- end

[download time left at zero]
file internal/postproc/stage_script.go
--- anchor
		DownloadTime:    int(downloadSecs),
--- replace
		DownloadTime:    0,
--- end

[average speed left at zero]
file internal/postproc/stage_script.go
--- anchor
		AvgBPS:          avgBPS,
--- replace
		AvgBPS:          avgBPS * 0,
--- end

[repair flag left at zero]
file internal/postproc/stage_script.go
--- anchor
		Repair:        boolInt(!shouldSkipForPP("repair", job.PP)),
--- replace
		Repair:        0,
--- end

[unpack flag left at zero]
file internal/postproc/stage_script.go
--- anchor
		Unpack:        boolInt(!shouldSkipForPP("unpack", job.PP)),
--- replace
		Unpack:        0,
--- end
