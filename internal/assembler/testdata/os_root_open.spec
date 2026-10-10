pkg ./internal/assembler/
run TestOpenTargetFile_RefusesToWriteOutOfTheJobDirectory$

# The writer opens a job's file through an os.Root on the job directory, so a
# symlink planted under the target's name, or a name that climbs out with "..",
# is refused at the open rather than followed to create or write a file
# outside it.

[the target opened by plain path]
file internal/assembler/assembler.go
--- anchor
	return root.OpenFile(name, os.O_WRONLY|os.O_CREATE, 0o644)
--- replace
	return os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE, 0o644)
--- end
