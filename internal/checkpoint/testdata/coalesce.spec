pkg ./internal/checkpoint/
run TestCheckpointer_CoalescesMarksIntoOneBatch

[Mark appends instead of keying by ID, so a job marked twice writes twice]
file internal/checkpoint/checkpointer.go
--- anchor
	c.dirty[j.ID()] = j
--- replace
	c.dirty[j.ID()+string(rune(len(c.dirty)))] = j
--- end

[Flush writes without clearing, so the next Flush rewrites the same rows]
file internal/checkpoint/checkpointer.go
--- anchor
	clear(c.dirty)
--- replace

--- end
