pkg ./internal/job/
run TestAllFetchPolicies_Exhaustive

[a declared policy dropped from the list]
file internal/job/progress.go
--- anchor
		FetchAlways,
		FetchIfNeeded,
		FetchNever,
--- replace
		FetchAlways,
		FetchIfNeeded,
--- end

[the list carrying a duplicate instead of the third value]
file internal/job/progress.go
--- anchor
		FetchAlways,
		FetchIfNeeded,
		FetchNever,
--- replace
		FetchAlways,
		FetchIfNeeded,
		FetchIfNeeded,
--- end
