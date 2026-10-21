pkg ./internal/app/
run TestAppResidency_HydrateThenEvict|TestHydrate_VerifiedJobRehydratesWithoutVerifying

[hydration always takes the verifying branch, re-reading a verified job's record]
file internal/app/residency.go
--- anchor
	if j.AllVerified() {
		return j.AttachArticles(m)
	}
--- replace
	if false {
		return j.AttachArticles(m)
	}
--- end
