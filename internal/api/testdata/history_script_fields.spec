pkg ./internal/api/
run ^TestHistoryDefault_ScriptLineButNoScriptLog$

# The handler copying the stored log into the slot is no longer a mutation: no
# history read selects script_log (history.entryColumns), on the paged path
# (Search) or the nzo_ids path (Get), so there is no log for the handler to
# copy. That half is pinned in internal/history by testdata/script_log_reads.spec.

[mode=history drops the script line]
file internal/api/history.go
--- anchor
			ScriptLine:   e.ScriptLine,
--- replace
			ScriptLine:   "",
--- end
