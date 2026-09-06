pkg ./internal/dispatch/
run TestOccupy_StopSkipsParkAndEvictOnOccupancyTimeout

[neutering waitLive in Stop]
file internal/dispatch/dispatch.go
--- anchor
			if err := d.waitLive(jobCtx, j.ID()); err != nil {
				// Wait timed out or context cancelled. Active occupiers for this job
				// have not drained. Skip Park and Evict to preserve memory safety and
				// prevent panics / null dereference under active finalization.
				stopErrs = append(stopErrs, fmt.Errorf("dispatch: Stop: wait live %s: %w", j.ID(), err))
				jobCancel()
				continue
			}
--- replace
			if false {
				continue
			}
--- end
