pkg ./internal/unpack/
run ^(TestFlatCollisions_GoUnRAR|TestFlatCollisions_GoSevenZip|TestFlatCollisions_GoTar|TestMemberPath)$

# Under OneFolder the in-process extractors keep every member whose flattened
# name collides: memberPath gives a file a uniquePath name, and a directory
# member creates nothing, so it can neither take a file's name (go_7z) nor
# fail the archive on one (go_tar) (#897). Each engine passes memberPath
# whether its member is a directory; those arguments are mutated one by one.

[a flat directory member is created]
file internal/unpack/sanitized_path.go
--- anchor
	if isDir {
		return "", false
	}
--- replace
	if false {
		return "", false
	}
--- end

[a flat file keeps a taken name]
file internal/unpack/sanitized_path.go
--- anchor
	return uniquePath(root, sp.Rel()), true
--- replace
	return sp.Rel(), true
--- end

[a flat file is renamed even with overwrite]
file internal/unpack/sanitized_path.go
--- anchor
	if opts.OverwriteFiles {
		return sp.Rel(), true
	}
--- replace
	if false {
		return sp.Rel(), true
	}
--- end

# Every extraction is staged, and the publish creates no directory under
# OneFolder, so a directory member read as a file is visible only when it
# comes before a file of the same flattened name and pushes it to x_1. 7z and
# `tar c b a` store the directory first; rar stores every directory record
# after its contents, so go_unrar's argument has no observable effect on an
# archive rar writes and is not mutated here.

[go_7z does not say a member is a directory]
file internal/unpack/go_sevenzip.go
--- anchor
	destRel, ok := memberPath(root, f.Name, f.FileInfo().IsDir(), opts, log)
--- replace
	destRel, ok := memberPath(root, f.Name, false, opts, log)
--- end

[go_tar does not say a directory member is one]
file internal/unpack/go_tar.go
--- anchor
		rel, ok := memberPath(root, hdr.Name, true, opts, log)
--- replace
		rel, ok := memberPath(root, hdr.Name, false, opts, log)
--- end

[go_tar says a file member is a directory]
file internal/unpack/go_tar.go
--- anchor
	destRel, ok := memberPath(root, hdr.Name, false, opts, log)
--- replace
	destRel, ok := memberPath(root, hdr.Name, true, opts, log)
--- end
