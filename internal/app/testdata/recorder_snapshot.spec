pkg ./internal/app/
run TestRecorder_CompleteNeverPrecedesItsRowUnderConcurrency

[the dirty snapshot moved into a second critical section]
file internal/app/record.go
--- anchor
	files := r.takeFilesLocked()
--- replace
	r.mu.Unlock()
	time.Sleep(time.Millisecond)
	r.mu.Lock()
	files := r.takeFilesLocked()
--- end
