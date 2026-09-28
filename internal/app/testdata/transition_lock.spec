pkg ./internal/app/
run TestRetryHistoryJob_(RefusesWhileAnotherHolderHasTheID|RefusesAJobTheDispatcherHolds)$|TestRemoveHistoryJob_ActsOnAFreshReadAfterAnInFlightRetry|TestMarkHistoryCompleted_WaitsForAnInFlightRetry|TestDeleteHistoryEntries_RefusesAnIDOutsideItsClaim|TestPruneHistory_SkipsAJobInTransition|TestRemoveJob_WaitsForAnInFlightTransition|TestFinalize_(WaitsForAnInFlightTransition|ProceedsWithoutTheLockOnceTheProcessIsStopping)$|TestStillExpired_KeepsOnlyHeldEntriesUnchangedSinceTheScan

# The transition lock's sites, each removed on its own. A claim site is
# neutered by claiming a different key, not by skipping the claim: that leaves
# the code compiling and releasing cleanly, so the only thing a mutation
# removes is the exclusion, and a kill has to come from the invariant's
# assertion. The guards around the claims are neutered by their condition.

[the retry claims a key no other actor uses]
file internal/app/app.go
--- anchor
	key := jobID
--- replace
	key := "mut-" + jobID
--- end

[the retry acts on a job the dispatcher already holds]
file internal/app/app.go
--- anchor
		if _, held := app.dispatcher.Job(jobID); held {
--- replace
		if _, held := app.dispatcher.Job(jobID); held && false {
--- end

[a history removal claims a key no other actor uses]
file internal/app/app.go
--- anchor
	claim, err := app.transitions.acquire(ctx, id)
	if err != nil {
		return fmt.Errorf("app: remove history %s: %w", id, err)
--- replace
	claim, err := app.transitions.acquire(ctx, "mut-"+id)
	if err != nil {
		return fmt.Errorf("app: remove history %s: %w", id, err)
--- end

[marking an entry completed claims a key no other actor uses]
file internal/app/app.go
--- anchor
	claim, err := app.transitions.acquire(ctx, id)
	if err != nil {
		return fmt.Errorf("app: mark history %s completed: %w", id, err)
--- replace
	claim, err := app.transitions.acquire(ctx, "mut-"+id)
	if err != nil {
		return fmt.Errorf("app: mark history %s completed: %w", id, err)
--- end

[the history choke point deletes entries its caller does not hold]
file internal/app/app.go
--- anchor
		if !claim.holds(entry.NzoID) {
--- replace
		if false {
--- end

# The key swap makes the claim hold none of the expired IDs, so the holds
# filter drops every entry: the test dies on the free entry surviving, not on
# the held one being deleted.
[the retention sweep claims keys no other actor uses]
file internal/app/app.go
--- anchor
		ids = append(ids, e.NzoID)
--- replace
		ids = append(ids, "mut-"+e.NzoID)
--- end

[the retention sweep deletes an entry filed again since its scan]
file internal/app/app.go
--- anchor
		if cur.Completed.Equal(e.Completed) {
--- replace
		if true {
--- end

[the retention sweep reports an entry already gone as a failure]
file internal/app/app.go
--- anchor
		if errors.Is(err, history.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("app: history retention: re-read %s: %w", e.NzoID, err)
--- replace
		if errors.Is(err, history.ErrNotFound) && false {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("app: history retention: re-read %s: %w", e.NzoID, err)
--- end

[a queue removal claims a key no other actor uses]
file internal/app/app.go
--- anchor
	claim, err := app.transitions.acquire(ctx, id)
	if err != nil {
		return fmt.Errorf("app: remove job %s: %w", id, err)
--- replace
	claim, err := app.transitions.acquire(ctx, "mut-"+id)
	if err != nil {
		return fmt.Errorf("app: remove job %s: %w", id, err)
--- end

[the finalizer claims a key no other actor uses]
file internal/app/job_finalizer.go
--- anchor
		claim, err := app.transitions.acquire(waitCtx, ppJob.Job.ID())
--- replace
		claim, err := app.transitions.acquire(waitCtx, "mut-"+ppJob.Job.ID())
--- end

# Dies on the elapsed-time assertion: the mutant waits out the whole cap.
[the finalizer's wait outlives a stopping process]
file internal/app/job_finalizer.go
--- anchor
		waitCtx, waitCancel := context.WithTimeout(app.ctx, finalizeTransitionWait)
--- replace
		waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(app.ctx), finalizeTransitionWait)
--- end
