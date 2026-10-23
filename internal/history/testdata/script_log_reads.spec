pkg ./internal/history/
run ^TestReads_DoNotLoadScriptLog$|^TestAddGetRoundTrip_EveryFieldDistinct$

# Re-adding script_log to a read takes two edits, one to entryColumns and one
# to scanEntry, and each alone fails every read with a column-count error, so
# neither is a mutation worth recording. These pin the two halves a single edit
# can break: that a read never returns the log's bytes, and that AddTx still
# stores them, since no read would notice if it stopped.

[a read returns script_log in another field's place]
file internal/history/repository.go
--- anchor
url, status, nzo_id, storage, path, script_line, download_time,
--- replace
url, status, nzo_id, storage, path, script_log, download_time,
--- end

[AddTx stops storing script_log]
file internal/history/repository.go
--- anchor
		e.ScriptLog, e.ScriptLine,
--- replace
		[]byte(nil), e.ScriptLine,
--- end
