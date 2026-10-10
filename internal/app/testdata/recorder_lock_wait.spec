pkg ./internal/app/
run TestRecorder_WriterLockWaitEndsWithTheWaitersCtx$

# The wait for wmu is bounded by each waiter's own ctx: an untrust on the
# assembler's worker must not wait out a holder that has no deadline.

[the writer lock is acquired ignoring the waiter's ctx]
file internal/app/record.go
--- anchor
	select {
	case r.wmu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
--- replace
	r.wmu <- struct{}{}
	return nil
--- end
