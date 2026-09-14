pkg ./internal/app/
run TestSweepOrphanedDurability_ReclaimsOnlyWhatNothingCanReach|TestSweepOrphanedDurability_SparesEveryJobWhenAnIDIsNull

[the FAILED exception dropped, so a retry's truncate bound is destroyed]
file internal/app/durability.go
--- anchor
   AND NOT EXISTS (SELECT 1 FROM history h
                    WHERE h.nzo_id = owned.job_id AND h.status = ?)`
--- replace
   AND (? <> '')`
--- end

[the queue exception dropped, so a live download's ground is swept]
file internal/app/durability.go
--- anchor
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = owned.job_id)
--- replace
 WHERE 1 = 1
--- end

[the sweep collects nothing, so every orphan survives]
file internal/app/durability.go
--- anchor
		orphans = append(orphans, id)
--- replace
		_ = id
--- end

[NOT EXISTS relaxed to NOT IN, the NULL trap the comment names]
file internal/app/durability.go
--- anchor
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = owned.job_id)
   AND NOT EXISTS (SELECT 1 FROM history h
                    WHERE h.nzo_id = owned.job_id AND h.status = ?)`
--- replace
 WHERE owned.job_id NOT IN (SELECT d.id FROM dispatch_jobs d)
   AND owned.job_id NOT IN (SELECT h.nzo_id FROM history h WHERE h.status = ?)`
--- end
