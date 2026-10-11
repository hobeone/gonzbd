pkg ./internal/api/
run TestQueueList_SizeTotalsIgnorePagination

[size totals summed over the page limit leaves (#749)]
file internal/api/queue.go
--- anchor
	for _, sl := range slots {
		totalQueueBytes += sl.Bytes
--- replace
	for _, sl := range slots[:min(len(slots), 1)] {
		totalQueueBytes += sl.Bytes
--- end

[size totals summed from start onwards (#749)]
file internal/api/queue.go
--- anchor
	for _, sl := range slots {
		totalQueueBytes += sl.Bytes
--- replace
	for _, sl := range slots[min(max(start, 0), len(slots)):] {
		totalQueueBytes += sl.Bytes
--- end
