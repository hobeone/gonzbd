pkg ./internal/queue/
run TestSQLiteStore_Par2ReleaseReasonSurvivesAReload|TestSQLiteStore_NonResidentJobKeepsItsPar2ReleaseReason|TestSQLiteStore_NonResidentJobKeepsItsDownloadStamps
timeout 5m

# The par2 release reason has to survive a restart, and the non-resident half
# is the one that bites: updateTx writes the column from JobProgress, so a job
# hydrated without a reason does not merely display nothing — it encodes the
# empty string back over the stored value. The download stamps hit exactly
# this, which is why their test runs here too: a mutation that breaks the
# shared carrier must kill both, and one of them passing alone would mean the
# carrier was special-cased rather than fixed.

[the resident restore drops the reason]
file internal/queue/sqlite_store.go
--- anchor
			job.setPar2ReleaseReason(par2Reason)
--- replace
			job.setPar2ReleaseReason("")
--- end

[the non-resident fill drops the reason, so the next update erases it]
file internal/queue/persistence.go
--- anchor
			job.setPar2ReleaseReason(fields.Par2ReleaseReason)
--- replace
			job.setPar2ReleaseReason(fields.Par2ReleaseReason[:0])
--- end

[the UPDATE stops carrying the reason]
file internal/queue/sqlite_store.go
--- anchor
		par2Reason = p.Par2ReleaseReason()
	}

	res, err := execer.ExecContext(ctx, q,
--- replace
		par2Reason = ""
	}

	res, err := execer.ExecContext(ctx, q,
--- end

[the bulk read stops selecting the reason]
file internal/queue/sqlite_store.go
--- anchor
			Par2ReleaseReason: par2Reason,
--- replace
			Par2ReleaseReason: "",
--- end
