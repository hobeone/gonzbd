pkg ./internal/dispatch/store/
run TestStore_

[Save drops the unwanted state]
file internal/dispatch/store/store.go
--- anchor
		p.Header.Unwanted,
--- replace
		0,
--- end
