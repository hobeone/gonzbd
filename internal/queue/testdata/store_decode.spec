pkg ./internal/queue/
run TestSQLiteStore_DownloadTimestampsPersistence

[the store decode stops restoring the stamps]
file internal/queue/sqlite_store.go
--- anchor
			job.progress.restoreDownloadStamps(
				time.Unix(dlStartedUnix, 0).UTC(), time.Unix(dlFinishedUnix, 0).UTC())
--- replace
			_, _ = dlStartedUnix, dlFinishedUnix
--- end
