pkg ./internal/app/
run TestAppResidency_HydrateThenEvict|TestHydrate_VerifiedJobRehydratesWithoutVerifying

[hydration always takes the verifying branch, re-reading a verified job's record]
file internal/app/residency.go
--- anchor
	if j.AllVerified() {
		_, err := j.AttachArticles(m, nil)
		return err
	}
--- replace
	if false {
		_, err := j.AttachArticles(m, nil)
		return err
	}
--- end
