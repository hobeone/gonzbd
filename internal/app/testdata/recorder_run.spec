pkg ./internal/app/
run TestRecorder_RunDoesNotFlushOnCancel

[run flushes when cancelled]
file internal/app/record.go
--- anchor
		case <-ctx.Done():
			return
--- replace
		case <-ctx.Done():
			_ = r.flush(context.Background())
			return
--- end
