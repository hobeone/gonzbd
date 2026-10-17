pkg ./internal/assembler/
run TestCheckDiskSpace_CountsOwnWritesBeforeTTL|TestDiskProbe_NoteWritten

[a written article is not counted against the free-space estimate]
file internal/assembler/assembler.go
--- anchor
		a.diskProbe.NoteWritten(o.n)
--- replace
		_ = o.n
--- end

[the estimate ignores the bytes written since the reading]
file internal/assembler/diskprobe.go
--- anchor
	return max(st.free-(p.wrote.Load()-st.wroteAtProbe), 0)
--- replace
	return st.free
--- end

[a fresh probe does not reset the subtraction]
file internal/assembler/diskprobe.go
--- anchor
		st.wroteAtProbe = p.wrote.Load()
--- replace
		_ = p.wrote.Load()
--- end

[the estimate is allowed to go negative]
file internal/assembler/diskprobe.go
--- anchor
	return max(st.free-(p.wrote.Load()-st.wroteAtProbe), 0)
--- replace
	return st.free - (p.wrote.Load() - st.wroteAtProbe)
--- end

[a caller waiting on an in-flight probe gets the raw reading]
file internal/assembler/diskprobe.go
--- anchor
		free, err := p.estimate(pending.st), pending.st.err
--- replace
		free, err := pending.st.free, pending.st.err
--- end

[bytes written during the probe are forgotten]
file internal/assembler/diskprobe.go
--- anchor
	st.free, st.err, st.fetchedAt = free, err, time.Now()
--- replace
	st.free, st.err, st.fetchedAt = free, err, time.Now()
	st.wroteAtProbe = p.wrote.Load()
--- end
