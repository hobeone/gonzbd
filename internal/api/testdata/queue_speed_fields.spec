pkg ./internal/api/
run TestQueueList_SpeedFields|TestQueueList_TimeleftZeroWhenPaused|TestFormatSpeed

[listing speed back to the literal (#734)]
file internal/api/queue.go
--- anchor
	speedStr, kbPerSec, timeleft := queueSpeedFields(speed, remainQueueBytes, paused)
--- replace
	_, kbPerSec, timeleft := queueSpeedFields(speed, remainQueueBytes, paused)
	speedStr := "0"
--- end

[listing kbpersec back to the literal (#734)]
file internal/api/queue.go
--- anchor
	speedStr, kbPerSec, timeleft := queueSpeedFields(speed, remainQueueBytes, paused)
--- replace
	speedStr, _, timeleft := queueSpeedFields(speed, remainQueueBytes, paused)
	kbPerSec := "0"
--- end

[listing timeleft back to the literal (#734)]
file internal/api/queue.go
--- anchor
	speedStr, kbPerSec, timeleft := queueSpeedFields(speed, remainQueueBytes, paused)
--- replace
	speedStr, kbPerSec, _ := queueSpeedFields(speed, remainQueueBytes, paused)
	timeleft := "0:00:00"
--- end

[detail variant leaves the three fields unset (#734)]
file internal/api/queue.go
--- anchor
	speedStr, kbPerSec, timeleft := queueSpeedFields(speed, slot.RemainingBytes, paused)
--- replace
	speedStr, kbPerSec, timeleft := "", "", ""
--- end

[timeleft ignores the global pause (#734)]
file internal/api/queue.go
--- anchor
	if !paused && speed > noiseFloorBPS && remaining > 0 {
		timeleft = formatDuration(int(float64(remaining) / speed))
--- replace
	if speed > noiseFloorBPS && remaining > 0 {
		timeleft = formatDuration(int(float64(remaining) / speed))
--- end

[formatSpeed skips the KB/s band]
file internal/api/helpers.go
--- anchor
	case bps < 1<<20:
--- replace
	case false:
--- end
