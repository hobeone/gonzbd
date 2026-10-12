# Red check for the default go test deadline (scripts/mutate/main.go's
# testArgs and testTimeout); how a timeout is classified is timeout.spec's.
#
#     go run ./scripts/mutate scripts/mutate/testdata/timeout_args.spec
pkg ./scripts/mutate/
run TestTestArgs_
timeout 5m

[a spec with no timeout line runs under go test's ten-minute default]
file scripts/mutate/main.go
--- anchor
	args = append(args, "-timeout", sp.testTimeout().String())
--- replace
	args = append(args, "-timeout", sp.timeout.String())
--- end

[a spec's own timeout is replaced by the default]
file scripts/mutate/main.go
--- anchor
	if sp.timeout > 0 {
		return sp.timeout
	}
--- replace
	if false && sp.timeout > 0 {
		return sp.timeout
	}
--- end
