pkg ./test/crash/
tags crash
run TestExternalModification_MtimeTouchCostsNoRefetch

[the row set goes missing entirely]
file test/crash/harness.go
--- anchor
		   FROM job_files WHERE job_id = ? ORDER BY file_index`, jobID)
--- replace
		   FROM job_files WHERE job_id = ? AND 0 ORDER BY file_index`, jobID)
--- end

# This one is killed by the per-row bounds check, NOT by the ordering clause
# beside it: every crash fixture submits one file, so index+1 is out of range
# before it can be out of order. The ordering clause has no fixture that
# reaches it and is stated as unexercised in JobFiles' own comment.
[every row reports an index one past its own]
file test/crash/harness.go
--- anchor
		`SELECT file_index, COALESCE(filename,''), complete
--- replace
		`SELECT file_index + 1, COALESCE(filename,''), complete
--- end
