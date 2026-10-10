pkg ./internal/app/
run ^TestInstallVerification_SettlesAndFailsBeforeThePeek$

# installVerification hands each file's whole outcome to
# job.Job.InstallFileVerification before the peek and the mark (#795). These
# mutations drop a part of that outcome from the hand-off; the job-side order
# within it is pinned by internal/job/testdata/install_file_verification.spec.

[a finished file is not settled before its peek]
file internal/app/residency.go
--- anchor
			Settle:        setComplete[fi],
--- replace
			Settle:        false,
--- end

[the intersection's losers are not failed]
file internal/app/residency.go
--- anchor
			Failed:        res.Failed[fi],
--- replace
			Failed:        nil,
--- end
