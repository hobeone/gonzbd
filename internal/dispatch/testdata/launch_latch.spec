pkg ./internal/dispatch/
run ^(TestClaimLaunched_ClaimsOnce|TestClaimLaunched_GuardsAgainstRemovingAndEvictedJobs|TestWorkerExit_ClearsTheLaunchedClaimSoALaterTickCanRelaunch|TestRemove_WaitsForActiveWorkerBeforeEvicting|TestStop_WaitsForActiveWorkersBeforeEviction|TestStop_WorkerTimeout_SkipsEvictionAndAggregatesErrors)$
timeout 1m

# The dispatcher's use of its launch latch (keyedlatch.Latch). The latch's own
# claim, release and wait are pinned by internal/keyedlatch/testdata/latch.spec;
# these mutations neuter each of the three dispatcher doors onto it.

[claimLaunched reports a claim it did not take]
file internal/dispatch/worker.go
--- anchor
	return d.launched.TryClaim(id)
--- replace
	d.launched.TryClaim(id)
	return true
--- end

[clearLaunched releases nothing]
file internal/dispatch/worker.go
--- anchor
	d.launched.Release(id)
--- replace
	_ = id
--- end

[waitLaunched does not wait]
file internal/dispatch/worker.go
--- anchor
	return d.launched.Wait(ctx, id)
--- replace
	return nil
--- end

[waitLaunched never gives up]
file internal/dispatch/worker.go
--- anchor
	return d.launched.Wait(ctx, id)
--- replace
	return d.launched.Wait(context.Background(), id)
--- end
