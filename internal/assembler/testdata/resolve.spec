pkg ./internal/assembler/
run TestFileWriter_Admit|TestFileWriter_Resolve|TestFileWriter_ResolveRefusesWhatWasNeverAdmitted|TestFileWriter_PartsIsTheSizeOfTheUnion|TestHandleSuccessArticle_RetryOfAFailedArticleIsNotCountedTwice|TestDuplicateSuccessIsNotReAcked|TestRejectedOffsetIsFailedAndStillCompletesTheFile|TestWriteFault_IsRoutedOutOfTheAssembler|TestWriteFault_IsNotCountedTowardCompletion|TestCollision_ArrivalRejectedOnceIncumbentIsWritten|TestLateDuplicate_ResolvesAnArticleTheWriterNeverAccepted|TestRouteOutcome_SplitsArticleFaultsFromStorageFaults|TestFaultedIncumbent_TakenOverThenRedeliveryRefused|TestFileWriter_RangeOwnersStayAdmitted

# The one entry and the one exit from the writer's accounting (#382): admit
# decides whether an admission charges a part, resolve applies the transition
# each disposition owes, and acceptArticle names a disposition on every return.

[admit charges a part for a retry of an article already counted as failed]
file internal/assembler/filewriter.go
--- anchor
	if !w.holdsPart(artIdx) {
		w.partsWritten++
	}
	w.seenDone[artIdx] = struct{}{}
	return true
--- replace
	w.partsWritten++
	w.seenDone[artIdx] = struct{}{}
	return true
--- end

[admit takes a duplicate of an accepted article on again]
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

[resolve stops refusing an article it never admitted]
file internal/assembler/filewriter.go
--- anchor
	if d != dispositionFetchFailed && !wasDone {
--- replace
	if false {
--- end

[resolve accepts the zero disposition silently]
file internal/assembler/filewriter.go
--- anchor
	default:
		panic(fmt.Sprintf("assembler: article %d resolved with %s, which is not a disposition", artIdx, d))
	}
--- replace
	default:
	}
--- end

[Rejected leaves the article in seenDone]
file internal/assembler/filewriter.go
--- anchor
		delete(w.seenDone, artIdx)
		w.seenFailed[artIdx] = struct{}{}
	case dispositionFetchFailed:
--- replace
		w.seenFailed[artIdx] = struct{}{}
	case dispositionFetchFailed:
--- end

[Rejected records no failure]
file internal/assembler/filewriter.go
--- anchor
		delete(w.seenDone, artIdx)
		w.seenFailed[artIdx] = struct{}{}
	case dispositionFetchFailed:
--- replace
		delete(w.seenDone, artIdx)
	case dispositionFetchFailed:
--- end

[FetchFailed charges a part for an article already holding one]
file internal/assembler/filewriter.go
--- anchor
		if !wasDone && !wasFailed {
			w.partsWritten++
		}
--- replace
		w.partsWritten++
--- end

[FetchFailed charges a part for an article already written]
file internal/assembler/filewriter.go
--- anchor
		if !wasDone && !wasFailed {
			w.partsWritten++
		}
--- replace
		if !wasFailed {
			w.partsWritten++
		}
--- end

[FetchFailed charges a part for a second copy of the same failure]
file internal/assembler/filewriter.go
--- anchor
		if !wasDone && !wasFailed {
			w.partsWritten++
		}
--- replace
		if !wasDone {
			w.partsWritten++
		}
--- end

[RolledBack gives back a part the failure holds]
file internal/assembler/filewriter.go
--- anchor
		delete(w.seenDone, artIdx)
		if !wasFailed {
			w.partsWritten--
		}
--- replace
		delete(w.seenDone, artIdx)
		w.partsWritten--
--- end

[RolledBack keeps the admission's part]
file internal/assembler/filewriter.go
--- anchor
		delete(w.seenDone, artIdx)
		if !wasFailed {
			w.partsWritten--
		}
--- replace
		delete(w.seenDone, artIdx)
--- end

[RolledBack leaves the article in seenDone]
file internal/assembler/filewriter.go
--- anchor
		delete(w.seenDone, artIdx)
		if !wasFailed {
			w.partsWritten--
		}
--- replace
		if !wasFailed {
			w.partsWritten--
		}
--- end

[an out-of-range offset is resolved RolledBack]
file internal/assembler/assembler.go
--- anchor
		return outcome{disposition: dispositionRejected, reason: reason, uncovered: WholeArticle}
--- replace
		return outcome{disposition: dispositionRolledBack, fault: errors.New(reason)}
--- end

[an owned-range collision is resolved Written]
file internal/assembler/assembler.go
--- anchor
		return outcome{
			disposition: dispositionRejected,
			reason:      "claims a byte range already written by another article",
			uncovered:   uncovered,
		}
--- replace
		return outcome{disposition: dispositionWritten, n: uncovered}
--- end

[a write fault is resolved Rejected]
file internal/assembler/assembler.go
--- anchor
		return outcome{disposition: dispositionRolledBack, fault: err}
--- replace
		return outcome{disposition: dispositionRejected, reason: err.Error(), uncovered: WholeArticle}
--- end

[handleSuccessArticle lets a duplicate of an accepted article past admit]
file internal/assembler/assembler.go
--- anchor
	if !w.admit(req.ArtIdx) {
--- replace
	if !w.admit(req.ArtIdx) && false {
--- end

[handleSuccessArticle resolves nothing]
file internal/assembler/assembler.go
--- anchor
	w.resolve(req.ArtIdx, o.disposition)
	a.routeOutcome(f, req, o)
--- replace
	a.routeOutcome(f, req, o)
--- end

[handleLateDuplicate drops a late article without resolving it]
file internal/assembler/assembler.go
--- anchor
	if a.opts.OnArticleRejected != nil {
		a.opts.OnArticleRejected(req.Owner, req.FileIdx, req.ArtIdx,
			"redelivered after its file was completed", WholeArticle)
--- replace
	if false {
		a.opts.OnArticleRejected(req.Owner, req.FileIdx, req.ArtIdx,
			"redelivered after its file was completed", WholeArticle)
--- end
