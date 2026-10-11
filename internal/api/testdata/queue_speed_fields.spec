pkg ./internal/api/
run TestQueueList_SpeedFields|TestQueueList_TimeleftZeroWhenPaused|TestQueueList_TimeleftZeroWhenNothingDownloads|TestFormatSpeed

[listing speed back to the literal (#734)]
file internal/api/queue.go
--- anchor
	speedStr, kbPerSec := queueSpeedFields(speed)
	timeleft := queueTimeleft(speed, remainQueueBytes, qStatus == "Downloading")
--- replace
	_, kbPerSec := queueSpeedFields(speed)
	speedStr := "0"
	timeleft := queueTimeleft(speed, remainQueueBytes, qStatus == "Downloading")
--- end

[listing kbpersec back to the literal (#734)]
file internal/api/queue.go
--- anchor
	speedStr, kbPerSec := queueSpeedFields(speed)
	timeleft := queueTimeleft(speed, remainQueueBytes, qStatus == "Downloading")
--- replace
	speedStr, _ := queueSpeedFields(speed)
	kbPerSec := "0"
	timeleft := queueTimeleft(speed, remainQueueBytes, qStatus == "Downloading")
--- end

[listing timeleft back to the literal (#734)]
file internal/api/queue.go
--- anchor
	timeleft := queueTimeleft(speed, remainQueueBytes, qStatus == "Downloading")
--- replace
	timeleft := "0:00:00"
--- end

[listing timeleft gated on the global pause alone]
file internal/api/queue.go
--- anchor
	timeleft := queueTimeleft(speed, remainQueueBytes, qStatus == "Downloading")
--- replace
	timeleft := queueTimeleft(speed, remainQueueBytes, !paused)
--- end

[queueTimeleft ignores downloading]
file internal/api/queue.go
--- anchor
	if !downloading || speed <= noiseFloorBPS || remaining <= 0 {
--- replace
	if speed <= noiseFloorBPS || remaining <= 0 {
--- end

[detail variant leaves speed and kbpersec unset (#734)]
file internal/api/queue.go
--- anchor
	speedStr, kbPerSec := queueSpeedFields(speed)
	timeleft := slot.Timeleft
--- replace
	speedStr, kbPerSec := "", ""
	timeleft := slot.Timeleft
--- end

[detail timeleft leaves timeleft unset (#734)]
file internal/api/queue.go
--- anchor
	timeleft := slot.Timeleft
--- replace
	timeleft := ""
--- end

[detail timeleft from the job's bytes whatever its status]
file internal/api/queue.go
--- anchor
	timeleft := slot.Timeleft
--- replace
	timeleft := queueTimeleft(speed, slot.RemainingBytes, !paused)
--- end

[formatSpeed skips the KB/s band]
file internal/api/helpers.go
--- anchor
	case bps < 1<<20:
--- replace
	case false:
--- end
