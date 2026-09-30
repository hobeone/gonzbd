pkg ./internal/app/
run TestHandleResult_DropsAResultFetchedForAnEarlierInstance

# The pipeline's one gate on a result's instance: a result fetched for an
# instance the dispatcher no longer holds under its ID reaches no consumer.

[the gate admits a result for any instance under the registered ID]
file internal/app/pipeline.go
--- anchor
	return ok && cur == res.Job
--- replace
	return ok && cur != nil
--- end
