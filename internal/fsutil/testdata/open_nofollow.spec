pkg ./internal/fsutil/
run TestOpenNoFollow$

# OpenNoFollow opens one path component inside a root with O_NOFOLLOW, so a
# symlink in the name's place is refused wherever it points, and a name that
# is not one component never reaches openat.

[O_NOFOLLOW dropped]
file internal/fsutil/nofollow_unix.go
--- anchor
	fd, err := unix.Openat(int(dir.Fd()), name, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
--- replace
	fd, err := unix.Openat(int(dir.Fd()), name, flag|unix.O_CLOEXEC, uint32(perm.Perm()))
--- end

[the one-component check dropped]
file internal/fsutil/nofollow_unix.go
--- anchor
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
--- replace
	if false && strings.ContainsRune(name, filepath.Separator) {
--- end
