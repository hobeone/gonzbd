pkg ./internal/app/
run TestRecorder_DropsRowsOfAReplacedInstance

[the instance check neutered]
file internal/app/record.go
--- anchor
	return r.current(j.ID()) == j
--- replace
	return true
--- end
