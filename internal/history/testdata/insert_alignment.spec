pkg ./internal/history/
run TestAddGetRoundTrip_EveryFieldDistinct

# The bug class this test exists for: two same-typed neighbours swapped in a
# hand-maintained positional list. Every mutation below compiles, because every
# argument is already an interface{} and every Scan dest is a pointer.

[two same-typed string arguments transposed in AddTx's INSERT]
file internal/history/repository.go
--- anchor
		e.Bytes, e.Meta, e.MD5Sum, e.Password,
--- replace
		e.Bytes, e.MD5Sum, e.Meta, e.Password,
--- end

[two same-typed int64 arguments transposed in AddTx's INSERT]
file internal/history/repository.go
--- anchor
		e.Downloaded, e.Completeness, e.FailMessage, e.URLInfo,
--- replace
		e.Completeness, e.Downloaded, e.FailMessage, e.URLInfo,
--- end

[two same-typed Scan destinations transposed in scanEntry]
file internal/history/repository.go
--- anchor
		&bytesVal, &meta, &md5sum, &password,
--- replace
		&bytesVal, &md5sum, &meta, &password,
--- end
