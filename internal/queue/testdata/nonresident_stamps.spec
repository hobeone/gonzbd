pkg ./internal/queue/
run TestSQLiteStore_NonResidentJobKeepsItsDownloadStamps

[Load stops restoring the stamps onto a hydrated non-resident job]
file internal/queue/persistence.go
--- anchor
			stamps := stampsByJob[job.ID]
			job.progress.restoreDownloadStamps(stamps.Started, stamps.Finished)
--- replace
			_ = stampsByJob[job.ID]
--- end

# A second mutation was tried here and removed: swapping decodeJobStamp for a
# bare time.Unix(v, 0).UTC() in DownloadStampsByJob. It SURVIVED, and it is an
# equivalent mutant rather than a coverage gap — restoreDownloadStamps applies
# isJobStamp to whatever it is handed, so an absent sentinel decoded as
# time.Unix(0, 0) is dropped at the door regardless. That redundancy is the
# gatekeeper argument for keeping the guard in restoreDownloadStamps, observed
# rather than asserted: the owner's check is what makes a caller's decoding
# mistake unobservable.
