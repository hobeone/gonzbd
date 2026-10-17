pkg ./internal/dispatch/store/
run TestStore_

[Save drops the unwanted state]
file internal/dispatch/store/store.go
--- anchor
		p.RecoveryBytes, p.Header.Unwanted,
--- replace
		p.RecoveryBytes, 0,
--- end
