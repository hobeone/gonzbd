pkg ./internal/assembler/
run TestCheckDiskSpace_CountsOwnWritesBeforeTTL|TestDiskProbe_NoteWritten

[a written article is not counted against the free-space estimate]
file internal/assembler/assembler.go
--- anchor
		a.diskProbe.NoteWritten(f.info.Dir, o.n)
--- replace
		_ = o.n
--- end

[the estimate ignores the bytes written since the reading]
file internal/assembler/diskprobe.go
--- anchor
	return max(st.free-(st.wrote-st.wroteAtProbe), 0)
--- replace
	return st.free
--- end

[a fresh probe does not reset the subtraction]
file internal/assembler/diskprobe.go
--- anchor
		st.wroteAtProbe = st.wrote
--- replace
		_ = st.wrote
--- end

[the estimate is allowed to go negative]
file internal/assembler/diskprobe.go
--- anchor
	return max(st.free-(st.wrote-st.wroteAtProbe), 0)
--- replace
	return st.free - (st.wrote - st.wroteAtProbe)
--- end

[bytes written during the probe are forgotten]
file internal/assembler/diskprobe.go
--- anchor
	st.free, st.err, st.fetchedAt = free, err, time.Now()
--- replace
	st.free, st.err, st.fetchedAt = free, err, time.Now()
	st.wroteAtProbe = st.wrote
--- end
