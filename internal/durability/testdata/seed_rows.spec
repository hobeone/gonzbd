pkg ./internal/durability/
run TestStore_SeedRowsCountsWrittenArticlesPerFile$

[a bad row fails the whole call]
run TestStore_SeedRowsIsolatesAJobWithAnUnreadableRow$
file internal/durability/progress.go
--- anchor
			unreadable[jobID] = err
			delete(rows, jobID)
			continue
--- replace
			return nil, nil, err
--- end

[a file with no written rows scans NULL, not 0]
file internal/durability/progress.go
--- anchor
COALESCE(w.n, 0)
--- replace
w.n
--- end

[a file with no written rows vanishes]
file internal/durability/progress.go
--- anchor
LEFT JOIN (SELECT job_id, file_idx, COUNT(*)
--- replace
JOIN (SELECT job_id, file_idx, COUNT(*)
--- end

[counts collapse to one per job]
file internal/durability/progress.go
--- anchor
GROUP BY job_id, file_idx) w
--- replace
GROUP BY job_id) w
--- end

[a count outside its range is accepted]
run TestSeedRowOf_RefusesAValueOutsideItsType$
file internal/durability/progress.go
--- anchor
		if !ok || v < 0 || v > col.max {
--- replace
		if !ok {
--- end

[a NULL filename reads as the empty name]
run TestSeedRowOf_RefusesAValueOutsideItsType$
file internal/durability/progress.go
--- anchor
	filename, ok := c[4].(string)
	if !ok {
--- replace
	filename, _ := c[4].(string)
	if false {
--- end

[fetch_policy is read from the complete column]
run TestSeedRowOf_AcceptsARowOfItsTypes$
file internal/durability/progress.go
--- anchor
		FetchPolicy:  uint8(n[3]), //nolint:gosec // G115: range-checked to 0-2 above
--- replace
		FetchPolicy:  uint8(n[2]), //nolint:gosec // G115: range-checked to 0-2 above
--- end
