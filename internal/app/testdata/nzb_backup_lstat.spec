pkg ./internal/app/
run TestWriteNZBBackup

# writeNZBBackup's uniqueness callback answers "is this name taken?", and Stat
# answers about a link's TARGET rather than the link. A dangling symlink in the
# backup directory therefore read as an unused name, and the backup took it,
# replacing the link.
#
# This is the weakest instance of the class the branch fixes -- nzbDir is
# <AdminDir>/nzb, which downloaded content never writes to, and the publish is a
# rename rather than a write through the name -- so it is a consistency fix
# rather than a containment one. It is mutated anyway because a name-allocation
# decision that is wrong about what occupies a name is worth pinning wherever it
# appears.
[the backup name check follows the link]
file internal/app/app.go
--- anchor
		_, err := os.Lstat(filepath.Join(nzbDir, candidate+".gz"))
--- replace
		_, err := os.Stat(filepath.Join(nzbDir, candidate+".gz"))
--- end
