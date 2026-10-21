pkg ./internal/durability/
run TestStore_SeedRowsCountsWrittenArticlesPerFile$

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
