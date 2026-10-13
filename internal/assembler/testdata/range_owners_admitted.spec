pkg ./internal/assembler/
run ^TestFileWriter_RangeOwnersStayAdmitted$

# ownedRanges owns a range against every arrival, its owner included, because
# an owner stays in seenDone and admit drops its redelivery (#930). Each arm
# breaks one half of that.

[admit lets a redelivery of an accepted article through]
file internal/assembler/filewriter.go
--- anchor
	if _, dup := w.seenDone[artIdx]; dup {
		return false
	}
--- replace
	if false {
		return false
	}
--- end

[Written takes the article out of seenDone]
file internal/assembler/filewriter.go
--- anchor
	case dispositionWritten:
		// The admission stands: seenDone kept, the part kept.
--- replace
	case dispositionWritten:
		delete(w.seenDone, artIdx)
--- end

[FetchFailed takes a written article out of seenDone]
file internal/assembler/filewriter.go
--- anchor
		if !wasDone && !wasFailed {
			w.partsWritten++
		}
--- replace
		if !wasDone && !wasFailed {
			w.partsWritten++
		}
		delete(w.seenDone, artIdx)
--- end

[a faulted write claims its range]
file internal/assembler/filewriter.go
--- anchor
	_, err := w.writeAt(data, off)
--- replace
	w.owned.claim(Range{Off: off, Len: int64(len(data))}, id)
	_, err := w.writeAt(data, off)
--- end
