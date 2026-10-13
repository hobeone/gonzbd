pkg ./internal/postproc/
run ^TestScriptStage_RecordsScriptLogAndLine$|^TestLastNonEmptyLineDirect$|^TestFormatScriptLine$

[the script stage records nothing]
file internal/postproc/stage_script.go
--- anchor
	job.recordScriptOutput(name, res)
--- replace
	job.recordScriptOutput(name, ScriptResult{})
--- end

[the log is not recorded]
file internal/postproc/stage_script.go
--- anchor
	j.scriptLog = res.LogBody
--- replace
	j.scriptLog = ""
--- end

[a clean run's last line is not used]
file internal/postproc/script.go
--- anchor
	case res.Err == nil && res.LastLine != "":
--- replace
	case false:
--- end

[a clean run with no output is not reported]
file internal/postproc/script.go
--- anchor
		return "Ran " + name
--- replace
		return ""
--- end

[a script that could not start reads as a non-zero exit]
file internal/postproc/script.go
--- anchor
	case !errors.Is(res.Err, ErrNonZeroExit):
--- replace
	case false:
--- end

[a non-zero exit drops its exit code]
file internal/postproc/script.go
--- anchor
		return fmt.Sprintf("Exit(%d): %s", res.ExitCode, res.LastLine)
--- replace
		return res.LastLine
--- end

[a silent non-zero exit is not reported]
file internal/postproc/script.go
--- anchor
		return fmt.Sprintf("Script exit code is %d", res.ExitCode)
--- replace
		return ""
--- end

[markup is kept]
file internal/postproc/script.go
--- anchor
	lines := strings.Split(markupTag.ReplaceAllString(s, " "), "\n")
--- replace
	lines := strings.Split(s, "\n")
--- end

[a trailing blank line hides the last line]
file internal/postproc/script.go
--- anchor
		if line == "" {
			continue
		}
--- replace
		if false {
			continue
		}
--- end

[a 150-rune line is not cut]
file internal/postproc/script.go
--- anchor
		if r := []rune(line); len(r) >= maxScriptLineRunes {
--- replace
		if r := []rune(line); len(r) > maxScriptLineRunes {
--- end

[tabs are not trimmed]
file internal/postproc/script.go
--- anchor
		line = strings.Trim(line, "\r\t ")
--- replace
		line = strings.Trim(line, "\r ")
--- end
