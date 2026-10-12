pkg ./internal/app/
run TestFailMsgForCounters_UnwordedAndNil

# A state failMsgForCounters' switch does not name is worded only when the
# caller says it is hopeless; with no progress there is no message at all.

[an unworded hopeless state gets no reason]
file internal/app/app.go
--- anchor
		if hopeless {
			return fmt.Sprintf(
				"Aborted: %.1f MB failed (%s). Job is beyond repair",
--- replace
		if false {
			return fmt.Sprintf(
				"Aborted: %.1f MB failed (%s). Job is beyond repair",
--- end

[every unworded state is worded]
file internal/app/app.go
--- anchor
		if hopeless {
			return fmt.Sprintf(
				"Aborted: %.1f MB failed (%s). Job is beyond repair",
--- replace
		if true {
			return fmt.Sprintf(
				"Aborted: %.1f MB failed (%s). Job is beyond repair",
--- end
