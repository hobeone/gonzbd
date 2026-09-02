pkg ./internal/app/
run TestPar2NeedsRecovery|TestMaybeReleaseRecoveryVolumes|TestApplyPar2Names

# The behavioural claims of the identify-then-verify wiring, at the two places
# it acts: the fetch decision, and the rename owner.

# The reversal itself. An entry matching nothing delivered must fetch the
# recovery volumes; the shipped code discarded them here, and that decision is
# terminal because post-processing cannot fetch anything (NeedRequeue has no
# consumer). Neutered as a switch case, which leaves id and idErr used.
[an unaccounted par2 entry no longer forces a fetch]
file internal/app/app.go
--- anchor
	case !id.Accounted():
--- replace
	case false:
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
		if err := app.queue.SetFileFilename(jobID, fi, r.To); err != nil &&
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
