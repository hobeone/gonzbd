pkg ./internal/app/
run TestRecorder_FlushesADirtyOnlyJob|TestRecorder_LeavesAJobFirstBufferedAfterTheListingForTheNextFlush

[a job with dirty state and no rows left out of the listing]
file internal/app/record.go
--- anchor
		if _, ok := r.pending[j]; !ok {
--- replace
		if false {
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
