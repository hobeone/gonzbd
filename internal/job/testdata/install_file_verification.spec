pkg ./internal/job/
run ^TestInstallFileVerification_

# Job.InstallFileVerification applies one file's verification outcome under a
# single contentMu hold (#795). Each mutation below breaks one step of it, or
# the order the steps must run in.

# The settle derives the CRC from the installed rows and releases them, so it
# must run last. Moved ahead of the install, it settles an empty file to zero
# and leaves the rows resident.
[the CRC is settled before the rows are installed]
file internal/job/verification.go
--- anchor
	installRows(m, p, fi, kept)
	if v.Complete {
		lo, hi := m.FileRange(fi)
		for i := lo; i < hi; i++ {
			_ = p.markFailed(m, i) // a no-op for an article a row just marked Done
		}
		p.files[fi].Complete = true
	}
	for _, a := range v.Failed {
		_ = j.markArticleFailed(int(a))
	}
	if v.Complete || v.Settle {
		settleFileCRC(m, p, fi)
	}
--- replace
	if v.Complete || v.Settle {
		settleFileCRC(m, p, fi)
	}
	installRows(m, p, fi, kept)
	if v.Complete {
		lo, hi := m.FileRange(fi)
		for i := lo; i < hi; i++ {
			_ = p.markFailed(m, i) // a no-op for an article a row just marked Done
		}
		p.files[fi].Complete = true
	}
	for _, a := range v.Failed {
		_ = j.markArticleFailed(int(a))
	}
--- end

[a finished file is not settled]
file internal/job/verification.go
--- anchor
	if v.Complete || v.Settle {
--- replace
	if v.Complete {
--- end

[the recorded filename is not restored]
file internal/job/verification.go
--- anchor
	if v.Filename != "" {
--- replace
	if false {
--- end

[the recorded policy is not restored when asked]
file internal/job/verification.go
--- anchor
	if v.RestorePolicy {
--- replace
	if false {
--- end

[the recorded policy is restored unasked]
file internal/job/verification.go
--- anchor
	if v.RestorePolicy {
--- replace
	if true {
--- end

[the intersection's losers are not failed]
file internal/job/verification.go
--- anchor
	for _, a := range v.Failed {
--- replace
	for _, a := range v.Failed[:0] {
--- end

# The checks come before any step, so a refused call changes nothing.
[the losers are failed before the file index is checked]
file internal/job/verification.go
--- anchor
	kept, dropped, err := placeRows(m, fi, v.Rows)
--- replace
	for _, a := range v.Failed {
		_ = j.markArticleFailed(int(a))
	}
	kept, dropped, err := placeRows(m, fi, v.Rows)
--- end

# The single hold itself is not pinned here. Releasing the lock partway through
# is caught by TestInstallFileVerification_NoReaderSeesHalfOfIt only when its
# reader is scheduled inside the window, which scripts/mutate reported FLAKY;
# a spec entry must be a deterministic kill.
