pkg ./internal/app/
run TestPipeline_HandleFailureResult_UnregisterableFileIsNotForwarded

# A permanent failure whose file cannot be registered goes back to the
# dispatch pool instead of reaching the assembler, which would raise a
# retryable "resolve" write fault and stall the job (#859).

[the failure is still marked and forwarded after the article is returned]
file internal/app/pipeline.go
--- anchor
			p.clearEmitted(res)
			return
		}

		// Record the permanent failure directly.
--- replace
			p.clearEmitted(res)
		}

		// Record the permanent failure directly.
--- end

[a failed registration is logged and ignored, as before the fix]
file internal/app/pipeline.go
--- anchor
			p.clearEmitted(res)
			return
		}

		// Record the permanent failure directly.
--- replace
		}

		// Record the permanent failure directly.
--- end
