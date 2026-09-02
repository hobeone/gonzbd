pkg ./internal/app/
run TestPar2NeedsRecovery|TestMaybeReleaseRecoveryVolumes|TestApplyPar2Names

# The behavioural claims of the identify-then-verify wiring, at the two places
# it acts: the fetch decision, and the rename owner.

# The reversal itself. An entry matching nothing delivered must fetch the
# recovery volumes when something else DID match; the shipped code discarded
# them here, and that decision is terminal because post-processing cannot fetch
# anything (NeedRequeue has no consumer). Neutered as a switch case, which
# leaves id and idErr used.
[an unaccounted par2 entry no longer forces a fetch]
file internal/app/app.go
--- anchor
	case !id.Accounted():
		names := make([]string, 0, len(id.Unaccounted))
--- replace
	case false:
		names := make([]string, 0, len(id.Unaccounted))
--- end

# The Layout B exemption, and the reason it is narrow. Widening it to every
# unaccounted set is the shape of the ORIGINAL defect -- a healthy obfuscated
# release also has entries that match nothing by name -- so a partially
# accounted job must still fetch. Dropping the len(id.Files) == 0 conjunct is
# exactly that widening.
[the layout B exemption swallows partially accounted jobs too]
file internal/app/app.go
--- anchor
	case !id.Accounted() && len(id.Files) == 0:
--- replace
	case !id.Accounted():
--- end

# The mirror: closing the exemption entirely sends every Layout B post to fetch
# a recovery set that cannot be spent, since RepairStage runs before
# UnpackStage and the protected files do not exist yet.
[the layout B exemption never fires]
file internal/app/app.go
--- anchor
	case !id.Accounted() && len(id.Files) == 0:
--- replace
	case false:
--- end

# Verification must read the names the renames just wrote. snap is a deep copy,
# so reusing its pre-rename progress compares obfuscated strings against the
# par2 index, matches nothing, and fetches the whole recovery set for an intact
# download -- the defect this path exists to fix, one line below the fix.
[verification reads the pre-rename snapshot]
file internal/app/app.go
--- anchor
				if fresh := app.queue.SnapshotJob(jobID); fresh != nil {
					prog = fresh.Progress()
				}
--- replace
				if fresh := app.queue.SnapshotJob(jobID); fresh != nil {
					_ = fresh
				}
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
