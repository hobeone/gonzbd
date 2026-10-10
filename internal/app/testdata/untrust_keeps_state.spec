pkg ./internal/app/
run TestHandleFileUntrusted_KeepsTheFilesNameForTheNextFlush

[the untrust no longer re-marks the file dirty]
file internal/app/record.go
--- anchor
		app.markFileDirty(j, fileIdx)
	}
	app.pipeline.forgetFile(jobID, fileIdx)
--- replace
	}
	app.pipeline.forgetFile(jobID, fileIdx)
--- end
