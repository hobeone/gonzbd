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
		return nil, storagefault.Classify("mkdir", info.Path, err)
--- replace
		a.releaseBuffer(req.Data); return nil, storagefault.Classify("mkdir", info.Path, err)
--- end

[the open failure releases the buffer again]
file internal/assembler/assembler.go
--- anchor
		return nil, storagefault.Classify("open", info.Path, err)
--- replace
		a.releaseBuffer(req.Data); return nil, storagefault.Classify("open", info.Path, err)
--- end

[processRequest stops releasing a failed open's buffer]
file internal/assembler/assembler.go
--- anchor
			a.noteWriteFault("", req, err)
--- replace
			a.noteWriteFault("", req, err); req.Data = nil
--- end
