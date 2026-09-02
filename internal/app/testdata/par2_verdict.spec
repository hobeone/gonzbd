pkg ./internal/app/
run TestPar2Verdict|TestMaybeReleaseRecoveryVolumes|TestApplyPar2Names|TestRecordPar2Names

# The behavioural claims of the identify-then-verify wiring, at the two places
# it acts: the fetch decision, and the rename recorder.

# The reversal itself. An entry matching nothing delivered must fetch the
# recovery volumes when something else DID match; the shipped code discarded
# them here, and that decision is terminal because post-processing cannot fetch
# anything (NeedRequeue has no consumer).
[an unaccounted par2 entry no longer forces a fetch]
file internal/app/app.go
--- anchor
	if !id.Accounted() {
		names := make([]string, 0, len(id.Unaccounted))
--- replace
	if false {
		names := make([]string, 0, len(id.Unaccounted))
--- end

# The Layout B exemption, and the reason it is narrow. Widening it to every
# unaccounted set is the shape of the ORIGINAL defect -- a healthy obfuscated
# release also has entries that match nothing by name -- so a partially
# accounted job must still fetch.
[the layout B exemption swallows partially accounted jobs too]
file internal/app/app.go
--- anchor
	if !id.Accounted() && len(id.Files) == 0 {
--- replace
	if !id.Accounted() {
--- end

# The mirror: closing the exemption entirely sends every Layout B post to fetch
# a recovery set that cannot be spent, since RepairStage runs before
# UnpackStage and the protected files do not exist yet.
[the layout B exemption never fires]
file internal/app/app.go
--- anchor
	if !id.Accounted() && len(id.Files) == 0 {
--- replace
	if false {
--- end

# Identification must actually run. Forcing the error branch would also fetch
# -- the conservative answer -- so instead make Accounted vacuously true, which
# is the shape of a matcher that silently finds nothing.
[identification always reports everything accounted]
file internal/par2/identify.go
--- anchor
func (id Identification) Accounted() bool { return len(id.Unaccounted) == 0 }
--- replace
func (id Identification) Accounted() bool { return true }
--- end

# The download path must not move files.
#
# It did, so that name-based verification would have corrected names to work
# with. Content identification made that pointless, and it was never free:
# JobProgress.Filename cannot hold a path, so a relocated file could not be
# recorded truthfully and the startup resume sweep stat'ed a top-level path
# that does not exist -- durability.Resume reads that as disproof of every run
# it holds and re-downloads a complete file. Relocation belongs to
# post-processing, ahead of the repair stage that needs the par2 paths.
[the download path relocates files]
file internal/app/app.go
--- anchor
			needsRecovery, reason = par2Verdict(a, app.log)
		}
--- replace
			needsRecovery, reason = par2Verdict(a, app.log)
			par2.ApplyRenames(dir, a, app.log)
		}
--- end

# (A mutation for "an assessment failure records no detail" belonged here and
# is deliberately absent. par2.Assess returns an error only when os.ReadDir
# fails, and any directory state that breaks it also breaks the
# FindPar2Files call one line earlier -- which takes the OTHER branch, so the
# fixture cannot reach the one under test. The aErr detail in the reason
# string is therefore an improvement this spec does not pin, rather than one
# it pins weakly; saying so is better than a mutation that reports SURVIVED
# or a fixture contorted until it lies.)
