pkg ./internal/app/
run ^(TestLooseRecord_UntrustedSurvivesEviction|TestLooseRecord_PoisonedSyncReturnsArticlesToOutstanding)$

# The syncFile seam reaches the assembler through New; production leaves it
# nil and the assembler fsyncs the handle itself.

[the seam is not handed to the assembler]
file internal/app/app.go
--- anchor
		SyncFile:            app.syncFile,
--- replace
		SyncFile:            nil,
--- end
