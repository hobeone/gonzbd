pkg ./internal/app/
run TestUntrustTimeout_LeavesTheCloseItsBudget$

# An untrust runs inside CloseJobHandles, so its bound ends before the close's.

[the untrust bound equals the close bound]
file internal/app/record.go
--- anchor
const untrustTimeout = 2 * time.Second
--- replace
const untrustTimeout = 5 * time.Second
--- end
