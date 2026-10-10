pkg ./internal/assembler/
run TestOpenTargetFile_RefusesToWriteOutOfTheJobDirectory$

# The writer opens a job's file with fsutil.OpenNoFollow on an os.Root on the
# job directory, so a symlink planted under the target's name, pointing out of
# the directory or at a sibling inside it, or a name that climbs out with
# "..", is refused at the open rather than followed to create or write
# another file.

[the target opened by plain path]
file internal/assembler/assembler.go
--- anchor
	return fsutil.OpenNoFollow(root, name, os.O_WRONLY|os.O_CREATE, 0o644)
--- replace
	return os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE, 0o644)
--- end

[the rooted open follows a link inside the job directory]
file internal/assembler/assembler.go
--- anchor
	return fsutil.OpenNoFollow(root, name, os.O_WRONLY|os.O_CREATE, 0o644)
--- replace
	return root.OpenFile(name, os.O_WRONLY|os.O_CREATE, 0o644)
--- end
