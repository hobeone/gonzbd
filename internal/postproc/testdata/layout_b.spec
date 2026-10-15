pkg ./internal/postproc/
run TestLayoutB_ArchiveExtractsDespitePar2NamingItsContents|TestLayoutB_DamagedArchiveFailsTheJob|TestQuickCheckStage_UnidentifiedNeedsEveryCondition|TestLayoutB_RepairOnlyJobKeepsRepair|TestLayoutA_ObfuscatedDamagedArchiveIsRepaired|TestQuickCheckStage_IdentifiedEntryStaysDamaged|TestHeldArchiveMembers_ScanErrorIsNotHeld|TestHeldArchiveMembers_ListingErrorIsNotHeld|TestRepairStage_DeclinesWhenQuickCheckUnidentified|TestRepairStage_HandlesEveryQuickCheckOutcome|TestQuickCheckStage_LayoutBRetryWithLeftoverExtractedFileDefersPar2Set

# The classification itself. Without it a Layout B post is Damaged, repair
# runs against files unpack has not produced, and ParError skips unpack.
[the unidentified classification is neutered]
file internal/postproc/stage_quickcheck.go
--- anchor
	case len(deferred) > 0 && len(deferred) == len(setsWithEntries(a.ID)):
--- replace
	case false:
--- end

# Condition 2: candidates drawn from Undelivered(). If Undelivered() is mutated
# to inspect only unaccounted entries, a retry with leftover extracted member
# stays Damaged instead of deferring to extracted_repair.
[undelivered fallback ignores identified files]
file internal/postproc/stage_quickcheck.go
--- anchor
	for _, fd := range a.Undelivered() {
--- replace
	for _, fd := range a.ID.Unaccounted {
--- end

# Condition 1, whole. Dropped, a PP=1 job or one with unpack disabled skips
# repair, skips unpack, and succeeds with nothing verified.
[the unpack-will-run condition is dropped]
file internal/postproc/stage_quickcheck.go
--- anchor
	if !q.unpackWillRun(job) {
--- replace
	if false {
--- end

# Condition 1, each term.
[unpackWillRun ignores the PP level]
file internal/postproc/stage_quickcheck.go
--- anchor
	return q.Unpack != nil && q.Unpack.IsEnabled() && !shouldSkipForPP(q.Unpack.Name(), job.PP)
--- replace
	return q.Unpack != nil && q.Unpack.IsEnabled()
--- end

[unpackWillRun ignores the stage toggle]
file internal/postproc/stage_quickcheck.go
--- anchor
	return q.Unpack != nil && q.Unpack.IsEnabled() && !shouldSkipForPP(q.Unpack.Name(), job.PP)
--- replace
	return q.Unpack != nil && !shouldSkipForPP(q.Unpack.Name(), job.PP)
--- end

# Condition 3. Dropped, a par2 set naming an archive that a delivered archive
# holds (outer.rar containing Real.Name.rar) would skip the repair of the
# archive par2 protects.
[the archive-named-entry condition is dropped]
file internal/postproc/stage_quickcheck.go
--- anchor
			return unpack.Classify(unpack.MemberBaseName(fd.FileName)) != unpack.UnknownArchive
--- replace
			return false
--- end

# Condition 4, whole: any RAR/7z present would do, including one that names
# none of the entries.
[the member-coverage condition is dropped]
file internal/postproc/stage_quickcheck.go
--- anchor
			if !held[unpack.MemberBaseName(fd.FileName)] {
--- replace
			if !held[unpack.MemberBaseName(fd.FileName)] && false {
--- end

# Condition 4's type filter widened: a split join or tar is listed, which
# MemberBaseNames refuses, so the whole check fails closed. Pinned by the
# Layout B RAR case going Damaged rather than by the split/tar cases.
[the archive filter accepts any archive]
file internal/postproc/stage_quickcheck.go
--- anchor
		if a.Type != unpack.RarArchive && a.Type != unpack.SevenZipArchive {
--- replace
		if a.Type == unpack.UnknownArchive {
--- end

# Condition 4's toggle filter dropped: a RAR with enable_unrar off (or a 7z
# with enable_7zip off) holds the entry, so repair defers to an unpack that
# will not extract it (#947).
[an archive whose type is disabled counts as held]
file internal/postproc/stage_quickcheck.go
--- anchor
		if !extracts(a.Type) {
--- replace
		if false {
--- end

[a listing error counts as held]
file internal/postproc/stage_quickcheck.go
--- anchor
			logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Cannot list archive members: %v — repair will run", err)
			return nil, false
--- replace
			logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Cannot list archive members: %v — repair will run", err)
			continue
--- end

[a failed archive scan counts as held]
file internal/postproc/stage_quickcheck.go
--- anchor
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Archive scan failed: %v — repair will run", err)
		return nil, false
--- replace
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Archive scan failed: %v — repair will run", err)
		return nil, true
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

# The par2-keep rule: without it the only thing that could check the
# extracted files against par2 is deleted before it has.
[par2_cleanup deletes an unverified deferred set]
file internal/postproc/stage_par2cleanup.go
--- anchor
	if len(job.deferredPar2Sets) > 0 && !job.deferredPar2Verified {
--- replace
	if false {
--- end
