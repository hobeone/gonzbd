pkg ./internal/app/
run TestAddJob_ChoosesAnotherNameWhileARetryHoldsItsName

# A name the registry refused is not offered again by the chooser.
[a refused name is chosen again]
file internal/app/rename.go
--- anchor
		refused[name] = true
--- replace
		_ = refused
--- end
