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

# THE ORDERING, which is what #494 is about.
#
# Recording the names before taking the verdict restores the sequence every
# defect came from: the renames land on disk, and the verdict is then computed
# from an assessment describing a directory that no longer exists. Under the
# old code this needed a re-snapshot to paper over; here it must simply be
# impossible to write correctly, and a test has to notice.
[the verdict is taken after the renames are applied]
file internal/app/app.go
--- anchor
			needsRecovery, reason = par2Verdict(a, app.log)
			app.recordPar2Names(jobID, dir, a, m, prog, app.log)
--- replace
			app.recordPar2Names(jobID, dir, a, m, prog, app.log)
			a2, _ := par2.AssessWithOptions(dir, sets, assembledFiles(m, prog), app.log, parseOpts)
			needsRecovery, reason = par2Verdict(a2, app.log)
--- end

# Owner half one: the file moves but nothing records where it went. This is the
# failure a test asserting only the on-disk rename cannot see, and it leaves
# JobProgress.Filename naming a path that no longer exists.
[the rename is not recorded on the queue]
file internal/app/par2names.go
--- anchor
		if err := app.queue.SetFileFilename(jobID, fi, to); err != nil &&
--- replace
		if err := error(nil); err != nil &&
--- end

# Owner half two: the resolved-name index is built from the manifest subject
# alone, ignoring any name already recorded. That silently breaks the second
# rename of the same file and is the plausible-looking simplification.
[the resolved-name index ignores previously recorded names]
file internal/app/par2names.go
--- anchor
		idxByName[resolvedName(m, p, fi)] = fi
--- replace
		idxByName[m.FileSubject(fi)] = fi
--- end
