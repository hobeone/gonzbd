pkg ./internal/postproc/
run TestLayoutB_ArchiveExtractsDespitePar2NamingItsContents|TestQuickCheckStage_NothingIdentifiedNeedsASelfVerifyingArchive|TestQuickCheckStage_IdentifiedEntryStaysDamaged|TestHasSelfVerifyingArchive_ScanErrorIsNoArchive|TestRepairStage_DeclinesWhenQuickCheckUnidentified|TestRepairStage_HandlesEveryQuickCheckOutcome

# The classification itself. Without it a Layout B post is Damaged, repair
# runs against files unpack has not produced, and ParError skips unpack.
[the unidentified classification is neutered]
file internal/postproc/stage_quickcheck.go
--- anchor
	case a.ID.NothingIdentified() && hasSelfVerifyingArchive(ctx, log, job):
--- replace
	case false:
--- end

# The archive half of the condition. Dropped, a payload with nothing to
# verify its extraction — a plain file, a split join, a tar — would skip
# repair on the same signature an obfuscated file damaged in its first 16 KB
# produces.
[the self-verifying-archive conjunct is dropped]
file internal/postproc/stage_quickcheck.go
--- anchor
	case a.ID.NothingIdentified() && hasSelfVerifyingArchive(ctx, log, job):
--- replace
	case a.ID.NothingIdentified():
--- end

# The identification half. Dropped, any damaged job carrying a RAR would skip
# repair, including one whose par2 set names the delivered volumes. The
# predicate's own terms are pinned in internal/par2/testdata/nothing_identified.spec.
[the nothing-identified conjunct is dropped]
file internal/postproc/stage_quickcheck.go
--- anchor
	case a.ID.NothingIdentified() && hasSelfVerifyingArchive(ctx, log, job):
--- replace
	case hasSelfVerifyingArchive(ctx, log, job):
--- end

# Widened to every archive type, a split join or tar would count as verifying
# its own extraction, which neither does.
[the archive filter accepts any archive]
file internal/postproc/stage_quickcheck.go
--- anchor
		if a.Type == unpack.RarArchive || a.Type == unpack.SevenZipArchive {
--- replace
		if a.Type != unpack.UnknownArchive {
--- end

# Each accepted type separately: 7z is the second self-verifying format.
[the archive filter drops 7z]
file internal/postproc/stage_quickcheck.go
--- anchor
		if a.Type == unpack.RarArchive || a.Type == unpack.SevenZipArchive {
--- replace
		if a.Type == unpack.RarArchive {
--- end

[the archive filter drops RAR]
file internal/postproc/stage_quickcheck.go
--- anchor
		if a.Type == unpack.RarArchive || a.Type == unpack.SevenZipArchive {
--- replace
		if a.Type == unpack.SevenZipArchive {
--- end

# A scan that failed proves nothing is there to verify anything.
[a failed archive scan counts as an archive]
file internal/postproc/stage_quickcheck.go
--- anchor
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Archive scan failed: %v", err)
		return false
--- replace
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Archive scan failed: %v", err)
		return true
--- end

# The repair stage's arm. Without it Unidentified falls to the default arm
# and runs par2 against files that do not exist yet.
[the repair stage's unidentified arm is removed]
file internal/postproc/stage_repair.go
--- anchor
	case QuickCheckUnidentified:
--- replace
	case QuickCheckOutcome(98):
--- end
