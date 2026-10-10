pkg ./internal/app/
run Test(Recorder_RunBoundsEachFlush|Recorder_RunFlushesOnTick|Recorder_RunDoesNotFlushOnCancel)$

# recorder.run holds wmu for each periodic flush; the assembler's worker waits
# for wmu in handleFileUntrusted, so each flush is bounded.

[the periodic flush has no deadline]
file internal/app/record.go
--- anchor
			fctx, cancel := context.WithTimeout(ctx, recorderFlushTimeout)
--- replace
			fctx, cancel := context.WithCancel(ctx)
--- end

[the periodic flush gets a far deadline]
file internal/app/record.go
--- anchor
			fctx, cancel := context.WithTimeout(ctx, recorderFlushTimeout)
--- replace
			fctx, cancel := context.WithTimeout(ctx, 100*recorderFlushTimeout)
--- end
