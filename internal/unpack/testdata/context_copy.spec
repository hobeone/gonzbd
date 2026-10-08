pkg ./internal/unpack/
run Test(ContextCopy_UsesPooledBuffer|ContextCopy_PeriodicCancellationCheck)$

[bypass contextCopyBufSize in contextCopyBufPool]
file internal/unpack/context_copy.go
--- anchor
		b := make([]byte, contextCopyBufSize)
--- replace
		b := make([]byte, 32*1024)
--- end

[mutate contextCopyCheckInterval from 256 KiB to 128 KiB]
file internal/unpack/context_copy.go
--- anchor
const contextCopyCheckInterval = 256 * 1024
--- replace
const contextCopyCheckInterval = 128 * 1024
--- end
