pkg ./internal/queue/
run TestJobStampCodec|TestSQLiteStore_DownloadTimestampsPersistence

[decode neutered]
file internal/queue/sqlite_store.go
--- anchor
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
--- replace
	_ = unix
	return time.Time{}
--- end

[encode drops the owner's bound]
file internal/queue/sqlite_store.go
--- anchor
	if !isJobStamp(t) {
		return 0
	}
	return t.Unix()
--- replace
	return t.Unix()
--- end

[encode guards on IsZero instead of the owner's bound]
file internal/queue/sqlite_store.go
--- anchor
	if !isJobStamp(t) {
		return 0
	}
	return t.Unix()
--- replace
	if t.IsZero() {
		return 0
	}
	return t.Unix()
--- end
