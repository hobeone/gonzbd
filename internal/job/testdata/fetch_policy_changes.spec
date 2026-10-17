pkg ./internal/job/
run TestTakeFetchPolicyChanges|TestInstallFileVerification_RestoreKeepsTheHold

# setFileFetchPolicy is the one writer of a file's fetch policy (#858). It
# records each change for the recorder, and stores exactly the policy it is
# given: a restored hold stays a hold.

[the writer records no change]
file internal/job/content.go
--- anchor
	if !f.fetchChanged {
		f.fetchChanged = true
--- replace
	if false {
		f.fetchChanged = true
--- end

[the take does not forget what it took]
file internal/job/content.go
--- anchor
		f.fetchChanged = false
--- replace
--- end

[a hold is turned into a release once a reason is recorded]
file internal/job/content.go
--- anchor
	f := &p.files[fi]
	if f.Fetch == policy {
--- replace
	if policy == FetchIfNeeded && p.par2ReleaseReason != "" {
		policy = FetchAlways
	}
	f := &p.files[fi]
	if f.Fetch == policy {
--- end

[the take hands over the policy alone]
file internal/job/content.go
--- anchor
		out = append(out, p.fileState(fi))
--- replace
		out = append(out, durability.FileState{FileIdx: fi, FetchPolicy: uint8(f.Fetch)})
--- end
