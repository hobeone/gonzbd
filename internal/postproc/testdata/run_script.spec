pkg ./internal/postproc/
run TestRunScript_ETXTBSY_RetriesThenReportsTheStartError|TestRunScript_LogCap

[an ETXTBSY start failure is not retried]
file internal/postproc/script.go
--- anchor
		if !isETXTBSY(startErr) {
--- replace
		if true {
--- end

[a capped log is not trimmed to a line boundary]
file internal/postproc/script.go
--- anchor
	if len(logBody) >= MaxLogBytes {
--- replace
	if false {
--- end

