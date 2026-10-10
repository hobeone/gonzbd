pkg ./internal/assembler/
run TestProcessRequest_FailedOpenReleasesTheBufferOnce

[the resolve failure releases the buffer again]
file internal/assembler/assembler.go
--- anchor
		return nil, storagefault.Classify("resolve", "", err)
--- replace
		a.releaseBuffer(req.Data); return nil, storagefault.Classify("resolve", "", err)
--- end

[the mkdir failure releases the buffer again]
file internal/assembler/assembler.go
--- anchor
		return nil, storagefault.Classify("mkdir", path, err)
--- replace
		a.releaseBuffer(req.Data); return nil, storagefault.Classify("mkdir", path, err)
--- end

[the open failure releases the buffer again]
file internal/assembler/assembler.go
--- anchor
		return nil, storagefault.Classify("open", path, err)
--- replace
		a.releaseBuffer(req.Data); return nil, storagefault.Classify("open", path, err)
--- end

[processRequest stops releasing a failed open's buffer]
file internal/assembler/assembler.go
--- anchor
			a.noteWriteFault("", req, err)
--- replace
			a.noteWriteFault("", req, err); req.Data = nil
--- end
