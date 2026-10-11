pkg ./internal/api/apitest/
run TestNopApp_Contract

[the aborted stub worker never reports back]
file internal/api/apitest/nopapp_test.go
--- anchor
		_ = w.d.YieldedJob(j)
--- replace
		_ = j
--- end
