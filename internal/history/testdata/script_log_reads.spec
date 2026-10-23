pkg ./internal/history/
run ^TestListReads_DoNotLoadScriptLog$|^TestColumnLists_DifferOnlyByScriptLog$|^TestScanEntry_ListShapeMapsColumnsByPosition$

[Search reads the full shape, so selects and scans script_log]
file internal/history/repository.go
--- anchor
	shape := listRow
	q := "SELECT "
--- replace
	shape := fullRow
	q := "SELECT "
--- end

[ExpiredEntries reads the full shape, so selects and scans script_log]
file internal/history/repository.go
--- anchor
	shape := listRow
	const expiredTail = `
--- replace
	shape := fullRow
	const expiredTail = `
--- end

[listColumns gains script_log]
file internal/history/repository.go
--- anchor
	listColumns = columnsBeforeLog + columnsAfterLog
--- replace
	listColumns = allColumns
--- end

[Get stops selecting script_log]
file internal/history/repository.go
--- anchor
	shape := fullRow
	q := `SELECT `
--- replace
	shape := listRow
	q := `SELECT `
--- end
