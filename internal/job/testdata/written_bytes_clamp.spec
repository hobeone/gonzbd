pkg ./internal/job/
run TestWrittenBytes

[written bytes does not clamp a torn read]
file internal/job/progress.go
--- anchor
	return max(expected-failed-remaining, 0)
--- replace
	return expected - failed - remaining
--- end
