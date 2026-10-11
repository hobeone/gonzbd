pkg ./internal/app/
run TestRecorder_FlushesADirtyOnlyJob|TestRecorder_LeavesAJobFirstBufferedAfterTheListingForTheNextFlush|TestRecorder_OwedDeleteIsWrittenOnItsOwn

[a job with dirty state and no rows left out of the listing]
file internal/app/record.go
--- anchor
	for j := range r.dirty {
		add(j)
	}
--- replace
	for j := range r.dirty {
		_ = j
	}
--- end

[a job with an owed delete and nothing buffered left out of the listing]
file internal/app/record.go
--- anchor
	for j := range r.owed {
		add(j)
	}
--- replace
	for j := range r.owed {
		_ = j
	}
--- end

[takeRowsLocked drops a job the listing did not check]
file internal/app/record.go
--- anchor
	for j, rows := range r.pending {
		current, checked := live[j]
		if !checked {
--- replace
	for j, rows := range r.pending {
		current, checked := live[j]
		if false && !checked {
--- end

[takeFilesLocked drops a job the listing did not check]
file internal/app/record.go
--- anchor
	for j, files := range r.dirty {
		current, checked := live[j]
		if !checked {
--- replace
	for j, files := range r.dirty {
		current, checked := live[j]
		if false && !checked {
--- end
