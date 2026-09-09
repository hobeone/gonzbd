pkg ./internal/history/
run TestOpen_TakesTheWriteLockAtBegin

[the _txlock=immediate DSN parameter dropped]
file internal/history/db.go
--- anchor
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
--- replace
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
--- end

[the DSN keeping the parameter but selecting deferred]
file internal/history/db.go
--- anchor
&_txlock=immediate"
--- replace
&_txlock=deferred"
--- end
